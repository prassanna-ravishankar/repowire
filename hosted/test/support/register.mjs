// Resolves `cloudflare:workers` to a stub so Durable Object classes load under Node.
import { registerHooks } from 'node:module';

const stub = new URL('./workers.mjs', import.meta.url).href;

registerHooks({
  resolve(specifier, context, next) {
    return specifier === 'cloudflare:workers' ? { url: stub, shortCircuit: true } : next(specifier, context);
  },
});
