import { DurableObject } from 'cloudflare:workers';
import { serial } from './serial';

export interface ExecResult { stdout: string; stderr: string; exitCode: number }

// What one command did, as the workspace saw it. `wall_ms` is container time,
// timed after the queue; `dirty` means the last checkpoint did not complete,
// so a stopped container cannot be restored safely; `snapshot_at` is the last
// successful snapshot.
export interface ExecOutcome {
  ok: boolean; result?: ExecResult; error?: string;
  wall_ms: number; snapshotted: boolean; dirty: boolean; snapshot_at: string | null;
}

// Workspace lifetime is independent of any agent. Commands sharing a workspace
// serialize with checkpointing so a snapshot never races another command.
export class Workspace extends DurableObject {
  private exclusive = serial();

  exec(command: string): Promise<ExecOutcome> {
    return this.exclusive(async () => {
      const started = Date.now();
      let snapshotted = false;
      const outcome = async (result: Pick<ExecOutcome, 'ok' | 'result' | 'error'>): Promise<ExecOutcome> => ({
        ...result, wall_ms: Date.now() - started, snapshotted,
        dirty: Boolean(await this.ctx.storage.get<boolean>('dirty')),
        snapshot_at: (await this.ctx.storage.get<string>('snapshotAt')) ?? null,
      });
      try {
        const container = this.ctx.container;
        if (!container) throw new Error('Workspace container binding is missing');
        if (!container.running) {
          if (await this.ctx.storage.get<boolean>('dirty')) {
            throw new Error('Workspace stopped before checkpointing; refusing to restore stale files');
          }
          const snapshotId = await this.ctx.storage.get<string>('snapshotId');
          container.start({
            ...(snapshotId ? { containerSnapshot: { id: snapshotId } } : { image: 'cloudflare/debian-trixie' }),
            entrypoint: ['sleep', 'infinity'],
            enableInternet: false,
          });
        }
        await container.setInactivityTimeout(60_000);
        await this.ctx.storage.put('dirty', true);
        const process = await container.exec(['timeout', '45', 'sh', '-c', command]);
        const output = await process.output();
        // Persist even a failed command's filesystem changes before stopping.
        const snapshot = await container.snapshotContainer({ name: 'workspace' });
        await this.ctx.storage.put({ snapshotId: snapshot.id, snapshotAt: new Date().toISOString(), dirty: false });
        snapshotted = true;
        await container.destroy();
        return outcome({
          ok: true,
          result: {
            stdout: new TextDecoder().decode(output.stdout).slice(0, 24000),
            stderr: new TextDecoder().decode(output.stderr).slice(0, 24000),
            exitCode: output.exitCode,
          },
        });
      } catch (error) {
        return outcome({ ok: false, error: error instanceof Error ? error.message : String(error) });
      }
    });
  }
}
