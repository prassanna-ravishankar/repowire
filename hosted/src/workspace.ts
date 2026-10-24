import { DurableObject } from 'cloudflare:workers';

// Workspace lifetime is independent of any agent. Commands sharing a workspace
// serialize with checkpointing so a snapshot never races another command.
export class Workspace extends DurableObject {
  private queue: Promise<unknown> = Promise.resolve();

  private exclusive<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.queue.then(operation);
    this.queue = result.catch(() => {});
    return result;
  }

  exec(command: string) {
    return this.exclusive(async () => {
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
      await this.ctx.storage.put({ snapshotId: snapshot.id, dirty: false });
      await container.destroy();
      return {
        stdout: new TextDecoder().decode(output.stdout).slice(0, 24000),
        stderr: new TextDecoder().decode(output.stderr).slice(0, 24000),
        exitCode: output.exitCode,
      };
    });
  }
}
