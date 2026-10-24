import { DurableObject } from 'cloudflare:workers';
import { Type } from '@earendil-works/pi-ai';
import { createModels } from '@earendil-works/pi-ai/models';
import { createRegistry, Harness } from '@earendil-works/pi-durable';
import { PiHarness } from 'agents/harness/pi';
import { Lifecycle } from 'agents/lifecycle';
import { createAI } from 'agents/models/pi-ai';
import { authenticate, body, HttpError, identifier, scopedName } from './http';
import { Workspace } from './workspace';
export { Workspace };

interface Env {
  AI: Ai;
  AGENTS: DurableObjectNamespace<HostedAgent>;
  WORKSPACES: DurableObjectNamespace<Workspace>;
  HOSTED_TOKEN: string;
  HOSTED_OWNER: string;
  MODEL: string;
}
interface AgentConfig { owner: string; id: string; workspace: string | null }

export class HostedAgent extends DurableObject<Env> {
  private ai = createAI({ binding: this.env.AI });
  private registry = createRegistry();
  private harness = new PiHarness({
    harness: ({ storage, context, onWake }) => {
      const models = createModels();
      models.setProvider(this.ai.provider);
      return Harness.open(storage, {
        models, registry: this.registry, onWake,
        settings: { afterFailedRun: 'withdraw' },
      }, context);
    },
    defaults: { model: this.ai(this.env.MODEL) },
  });
  private lifecycle = Lifecycle.install(this).use(this.harness);

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.registry.install({
      name: 'repowire',
      sections: [{ key: 'preamble', tag: false, render: () =>
        'You are a hosted Repowire agent. Answer the requested task and stop when done. Your conversation persists between requests. A workspace, when attached, is shared with other agents. Shell commands have no network access and a 45 second timeout. Do not claim work was performed unless tools confirm it.' }],
      tools: [{
        name: 'workspace_exec',
        description: 'Execute a shell command in the attached Linux workspace. Files persist between calls. No network access.',
        parameters: Type.Object({ command: Type.String({ maxLength: 16000 }) }),
        // Shell side effects must not be blindly replayed after interruption.
        execute: async ({ command }) => {
          const config = await this.config();
          if (!config?.workspace) throw new Error('This agent has no workspace attached');
          const workspace = this.env.WORKSPACES.getByName(scopedName(config.owner, config.workspace));
          const result = await workspace.exec(command);
          return { output: [{ type: 'text' as const, text: JSON.stringify(result) }] };
        },
      }],
    });
  }

  config() { return this.ctx.storage.get<AgentConfig>('config'); }

  async spawn(config: AgentConfig) {
    return this.ctx.blockConcurrencyWhile(async () => {
      const existing = await this.config();
      if (existing) return { created: false, agent: existing };
      await this.ctx.storage.put('config', config);
      return { created: true, agent: config };
    });
  }

  async submit(text: string, operationId: string) {
    if (!await this.config()) throw new Error('Agent does not exist');
    return this.harness.submit(text, { operationId });
  }
  async status() {
    return { agent: await this.config(), busy: await this.harness.session().busy(), pending: await this.harness.pending() };
  }
  wait(operationId: string) { return this.harness.wait(operationId); }
  async messages(): Promise<string> { return JSON.stringify(await this.harness.messages()); }
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    try {
      const url = new URL(request.url);
      if (url.pathname === '/health' && request.method === 'GET') return Response.json({ ok: true });
      await authenticate(request, env.HOSTED_TOKEN);
      // Single-owner private spike. The caller never chooses the tenant.
      if (!env.HOSTED_OWNER) throw new HttpError(503, 'Hosted owner is not configured');
      if (url.pathname === '/agents' && request.method === 'POST') {
        const input = await body(request);
        const id = identifier(input.id, 'id');
        const workspace = input.workspace == null ? null : identifier(input.workspace, 'workspace');
        const agent = env.AGENTS.getByName(scopedName(env.HOSTED_OWNER, id));
        const result = await agent.spawn({ owner: env.HOSTED_OWNER, id, workspace });
        if (result.agent.workspace !== workspace) throw new HttpError(409, 'Agent already exists with a different workspace');
        return Response.json(result, { status: result.created ? 201 : 200 });
      }
      const match = /^\/agents\/([^/]+)(?:\/(messages|operations)(?:\/([^/]+))?)?$/.exec(url.pathname);
      if (!match) throw new HttpError(404, 'Not found');
      const id = identifier(match[1], 'id');
      const agent = env.AGENTS.getByName(scopedName(env.HOSTED_OWNER, id));
      if (!await agent.config()) throw new HttpError(404, 'Agent not found');
      if (!match[2] && request.method === 'GET') return Response.json(await agent.status());
      if (match[2] === 'messages' && !match[3]) {
        if (request.method === 'GET') return new Response(await agent.messages(), { headers: { "Content-Type": "application/json" } });
        if (request.method === 'POST') {
          const input = await body(request);
          const operationId = identifier(input.operationId, 'operationId');
          if (typeof input.text !== 'string' || !input.text.trim() || input.text.length > 16000) {
            throw new HttpError(400, 'text must contain 1–16000 characters');
          }
          return Response.json(await agent.submit(input.text, operationId), { status: 202 });
        }
      }
      if (match[2] === 'operations' && match[3] && request.method === 'GET') {
        return Response.json(await agent.wait(identifier(match[3], 'operationId')));
      }
      throw new HttpError(405, 'Method not allowed');
    } catch (error) {
      if (error instanceof HttpError) return Response.json({ error: error.message }, { status: error.status });
      console.error('Hosted request failed', error);
      return Response.json({ error: 'Hosted operation failed; inspect Worker logs' }, { status: 500 });
    }
  },
} satisfies ExportedHandler<Env>;
