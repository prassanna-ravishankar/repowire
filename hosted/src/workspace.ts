import { DirectoryBackup, type DirectoryBackupGatewayBinding, type DirectoryBackupRecord } from '@cloudflare/sandbox';
import { DurableObject } from 'cloudflare:workers';
import { serial } from './serial';

export interface ExecResult { stdout: string; stderr: string; exitCode: number }

// What one command did, as the workspace saw it. `wall_ms` is container time,
// timed after the queue. `snapshot_at` is when the files were last saved, and
// `dirty` means the last save failed, so changes since then are at risk.
// `lost` means the container had stopped, or was cut off mid-command, with
// changes that were never saved: the command ran on the last save instead.
// `legacy` means the files from before directory backups (a container
// snapshot) were not carried over.
export interface ExecOutcome {
  ok: boolean; result?: ExecResult; error?: string;
  wall_ms: number; dirty: boolean; snapshot_at: string | null; lost?: boolean; legacy?: boolean;
}

// How a turn-end save went. `dirty` means changes since the last save are at
// risk: the save failed, or the container that held them is gone (`lost`).
export interface SaveResult { saved: boolean; dirty: boolean; snapshot_at: string | null; lost?: boolean }

interface Env { WORKSPACE_BACKUPS: R2Bucket }
interface WorkspaceState extends DurableObjectState {
  readonly exports: Cloudflare.Exports & { readonly DirectoryBackupGateway: DirectoryBackupGatewayBinding };
}
// The last save of /workspace, as DirectoryBackup recorded it.
interface Saved { backup: DirectoryBackupRecord; at: string }
// What a workspace needs from DirectoryBackup; a seam for tests.
export type Backups = Pick<DirectoryBackup, 'backup' | 'restore' | 'delete'>;

const DIR = '/workspace';
// While agents are working the container stays up between their commands;
// once a turn has been saved it stops soon unless another command comes.
const WORKING_IDLE_MS = 30 * 60_000;
const SAVED_IDLE_MS = 2 * 60_000;
const COMMAND_TIMEOUT_S = 45;

// A workspace is a Linux sandbox that agents attach to by its id
// (@cloudflare/sandbox 1.0: a Durable Object that owns its Container). Its
// lifetime is independent of any agent. The container runs while agents use
// it; its files are saved to R2 when a turn ends and restored when the
// container next starts. Commands and saves take turns, so a save never
// captures a command half done.
export class Workspace extends DurableObject<Env> {
  private exclusive = serial();
  // Whether this instance has taken charge of the running container. Work
  // in the container outlives the object: one that restarts mid-command
  // finds the `busy` marker it left and cannot know how that command ended.
  private adopted = false;

  protected backups(): Backups {
    const ctx = this.ctx as WorkspaceState;
    return new DirectoryBackup(this.container(), ctx.exports.DirectoryBackupGateway, { binding: 'WORKSPACE_BACKUPS', prefix: 'workspaces/' });
  }

  private container(): Container {
    const container = this.ctx.container;
    if (!container) throw new Error('Workspace container binding is missing');
    return container;
  }

  private saved() { return this.ctx.storage.kv.get<Saved>('saved'); }

  // Takes charge of the container: one cut off mid-command by a restart of
  // this object is replaced, since what it holds is unknown. A container that
  // outlived an idle eviction is kept as it is.
  private async adopt(container: Container) {
    const kv = this.ctx.storage.kv;
    if (!this.adopted && container.running && kv.get('busy')) await container.destroy();
    kv.delete('busy');
    this.adopted = true;
  }

  // A running container with /workspace in place, restored from the last
  // save when it is new. Says whether unsaved changes or pre-backup files
  // were lost on the way.
  private async ready(container: Container): Promise<{ lost: boolean; legacy: boolean }> {
    const kv = this.ctx.storage.kv;
    await this.adopt(container);
    if (container.running) return { lost: false, legacy: false };
    // Changes made since the last save are gone with the container that
    // held them.
    const lost = Boolean(kv.get<boolean>('unsaved'));
    const legacy = kv.get('snapshotId') !== undefined;
    container.start({ image: container.images.sandbox, enableInternet: false });
    kv.put('busy', 'restore');
    try {
      // /workspace is made here, in the container's writable layer: a
      // restore swaps the directory in, and overlayfs cannot rename one
      // that an image layer holds.
      const saved = this.saved();
      if (saved) await this.backups().restore(saved.backup);
      else await (await container.exec(['mkdir', '-p', DIR])).output();
    } catch (error) {
      // A half-restored container must not take commands or be saved over
      // the good copy: the next command starts again.
      await container.destroy().catch(() => {});
      throw error;
    } finally {
      kv.delete('busy');
    }
    this.ctx.storage.transactionSync(() => {
      kv.delete('unsaved');
      for (const key of ['snapshotId', 'snapshotAt', 'dirty']) kv.delete(key);
    });
    return { lost, legacy };
  }

  exec(command: string, agent: string): Promise<ExecOutcome> {
    return this.exclusive(async () => {
      const kv = this.ctx.storage.kv;
      const started = Date.now();
      kv.put('lastExecAt', started);
      let notes: { lost?: boolean; legacy?: boolean } = {};
      const outcome = (result: Pick<ExecOutcome, 'ok' | 'result' | 'error'>): ExecOutcome => ({
        ...result, wall_ms: Date.now() - started, dirty: Boolean(kv.get<boolean>('saveFailed')),
        snapshot_at: this.saved()?.at ?? null, ...notes,
      });
      try {
        const container = this.container();
        const { lost, legacy } = await this.ready(container);
        notes = { ...(lost ? { lost } : {}), ...(legacy ? { legacy } : {}) };
        await container.setInactivityTimeout(WORKING_IDLE_MS);
        kv.put('unsaved', true);
        kv.put('busy', 'exec');
        try {
          const process = await container.exec(['timeout', String(COMMAND_TIMEOUT_S), 'sh', '-c', command], {
            cwd: DIR, env: { REPOWIRE_AGENT: agent },
          });
          const output = await process.output();
          return outcome({
            ok: true,
            result: {
              stdout: new TextDecoder().decode(output.stdout).slice(0, 24000),
              stderr: new TextDecoder().decode(output.stderr).slice(0, 24000),
              exitCode: output.exitCode,
            },
          });
        } finally {
          kv.delete('busy');
        }
      } catch (error) {
        return outcome({ ok: false, error: error instanceof Error ? error.message : String(error) });
      }
    });
  }

  // Saves /workspace once a turn ends, if a command changed it since the last
  // save. The new save replaces the old, whose object is then deleted. A
  // failed save keeps the changes in the running container for the next save,
  // and a container that stopped first has lost them: both are reported.
  checkpoint(): Promise<SaveResult> {
    return this.exclusive(async () => {
      const kv = this.ctx.storage.kv;
      const result = (saved: boolean, lost = false): SaveResult => ({
        saved, dirty: lost || Boolean(kv.get<boolean>('saveFailed')), snapshot_at: this.saved()?.at ?? null, ...(lost ? { lost } : {}),
      });
      const container = this.container();
      await this.adopt(container);
      if (!kv.get<boolean>('unsaved')) return result(false);
      // The next command reports the loss too, as it starts over.
      if (!container.running) return result(false, true);
      const previous = this.saved();
      const backups = this.backups();
      let backup: DirectoryBackupRecord;
      kv.put('busy', 'save');
      try {
        backup = await backups.backup({ dir: DIR });
      } catch (error) {
        kv.put('saveFailed', true);
        throw error;
      } finally {
        kv.delete('busy');
      }
      this.ctx.storage.transactionSync(() => {
        kv.put<Saved>('saved', { backup, at: new Date().toISOString() });
        kv.delete('unsaved');
        kv.delete('saveFailed');
      });
      if (previous) await backups.delete(previous.backup).catch((error) => console.error('Workspace: old save not deleted', error));
      await container.setInactivityTimeout(SAVED_IDLE_MS);
      return result(true);
    });
  }
  // Deletes this workspace's files for good, when no agent uses it any more
  // (the owner's Gateway decides; rfcs/retention.md, "Hosted"): the saved
  // copy in R2, then this object's storage. In turn with commands, and never
  // while a container runs. The Gateway decided from what it knew; a command
  // that ran here since `idleSinceMs` means the workspace is in use again, so
  // it is kept.
  purge(idleSinceMs: number): Promise<boolean> {
    return this.exclusive(async () => {
      if (this.ctx.container?.running) return false;
      if ((this.ctx.storage.kv.get<number>('lastExecAt') ?? 0) >= idleSinceMs) return false;
      const saved = this.saved();
      if (saved) await this.backups().delete(saved.backup);
      await this.ctx.storage.deleteAll();
      return true;
    });
  }
}
