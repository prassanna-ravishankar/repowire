import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Type } from '@earendil-works/pi-ai';
import { createModels } from '@earendil-works/pi-ai/models';
import { fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai/providers/faux';
import { BACKGROUND_CONTEXT as BG } from '@earendil-works/chord/context';
import { createRegistry, GenerationTask, Harness, hook } from '@earendil-works/pi-durable';
import { MemoryStorage } from '@earendil-works/pi-durable/storage/memory';
import { stepLimit } from '../src/steps';

// A model that never stops calling a tool, against real pi scheduling.
test('the step limit stops a tool loop before its next request is sent', async () => {
  const faux = fauxProvider();
  let sent = 0;
  const loop = () => {
    sent++;
    return fauxAssistantMessage(fauxToolCall('noop', {}), { stopReason: 'toolUse' });
  };
  faux.setResponses(Array.from({ length: 50 }, () => loop));
  const models = createModels();
  models.setProvider(faux.provider);

  const steps = new Map<string, number>();
  const registry = createRegistry();
  let pi!: Harness;
  registry.install({
    name: 'test',
    hooks: [hook(GenerationTask, {
      beforeRequest: stepLimit({
        running: async () => (await pi.inspect(BG)).submissions
          .filter((record) => record.status !== 'queued' && record.requestId !== undefined)
          .map((record) => record.requestId!),
        count: (operationId) => {
          steps.set(operationId, (steps.get(operationId) ?? 0) + 1);
          return steps.get(operationId)!;
        },
        // As PiHarness withdraws an operation that already joined the run.
        abort: async (operationId) => {
          const record = (await pi.inspect(BG)).submissions.find((item) => item.requestId === operationId);
          if (!record) return;
          if (await pi.abortSubmission(record.id, BG) === 'already_placed') await (await pi.conversation(record.conversationId, BG))?.abort(BG);
        },
      }, 3),
    })],
    tools: [{ name: 'noop', description: 'Does nothing.', parameters: Type.Object({}), execute: async () => ({ output: [{ type: 'text' as const, text: 'ok' }] }) }],
  });
  pi = await Harness.open(new MemoryStorage(), { models, registry }, BG);
  const model = faux.getModel();
  const root = await pi.root(BG, { agent: { model: { provider: model.provider, modelId: model.id } } });
  const submission = await root.submit({ type: 'input', content: 'loop', requestId: 'op-1' }, BG);
  const settled = await submission.wait(BG);

  assert.equal(sent, 3);
  assert.equal(steps.get('op-1'), 4);
  assert.equal(settled.status, 'unanswered');
  await pi.close?.(BG);
});
