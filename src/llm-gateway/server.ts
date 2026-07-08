/**
 * llm-gateway — an OpenAI-compatible HTTP facade over the Claude Code CLI.
 *
 * Exposes:
 *   GET  /healthz              — liveness probe
 *   GET  /v1/models            — models advertised to clients
 *   POST /v1/chat/completions  — OpenAI Chat Completions (stream and non-stream)
 *
 * Every completion spawns one `claude -p` process (see claude-cli.ts), so any
 * client that speaks the OpenAI API — openai SDKs, Open WebUI, LiteLLM,
 * LangChain — can use whatever Claude auth the CLI is logged in with.
 *
 * Run: bun src/llm-gateway/server.ts   (or: node --experimental-strip-types)
 *
 * Environment:
 *   CLAUDE_GATEWAY_PORT         listen port (default 8787)
 *   CLAUDE_GATEWAY_HOST         bind address (default 127.0.0.1)
 *   CLAUDE_GATEWAY_API_KEY      if set, required as Bearer token / x-api-key
 *   CLAUDE_GATEWAY_MODEL        default model when the request omits one (default "sonnet")
 *   CLAUDE_GATEWAY_MODELS       comma-separated list served by /v1/models
 *   CLAUDE_GATEWAY_BIN          path to the claude executable (default "claude")
 *   CLAUDE_GATEWAY_TIMEOUT_MS   per-request CLI timeout (default 300000)
 *   CLAUDE_GATEWAY_CONCURRENCY  max simultaneous CLI processes (default 4)
 */

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { ClaudeCliError, runClaude } from './claude-cli.js';
import {
  BadRequestError,
  buildPrompt,
  completionResponse,
  mapFinishReason,
  newCompletionId,
  streamChunk,
  toOpenAIUsage,
  type ChatCompletionRequest,
} from './openai-compat.js';

const config = {
  port: Number(process.env.CLAUDE_GATEWAY_PORT ?? 8787),
  host: process.env.CLAUDE_GATEWAY_HOST ?? '127.0.0.1',
  apiKey: process.env.CLAUDE_GATEWAY_API_KEY ?? '',
  defaultModel: process.env.CLAUDE_GATEWAY_MODEL ?? 'sonnet',
  models: (process.env.CLAUDE_GATEWAY_MODELS ?? 'sonnet,opus,haiku').split(',').map((m) => m.trim()).filter(Boolean),
  claudeBin: process.env.CLAUDE_GATEWAY_BIN ?? 'claude',
  timeoutMs: Number(process.env.CLAUDE_GATEWAY_TIMEOUT_MS ?? 300_000),
  concurrency: Number(process.env.CLAUDE_GATEWAY_CONCURRENCY ?? 4),
};

/** FIFO semaphore so a burst of requests doesn't fork-bomb the host with CLI processes. */
class Semaphore {
  private available: number;
  private readonly waiters: Array<() => void> = [];

  constructor(size: number) {
    this.available = Math.max(1, size);
  }

  async acquire(): Promise<void> {
    if (this.available > 0) {
      this.available--;
      return;
    }
    await new Promise<void>((resolve) => this.waiters.push(resolve));
  }

  release(): void {
    const next = this.waiters.shift();
    if (next) next();
    else this.available++;
  }
}

const slots = new Semaphore(config.concurrency);

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(payload),
  });
  res.end(payload);
}

function sendError(res: ServerResponse, status: number, message: string, type = 'invalid_request_error'): void {
  sendJson(res, status, { error: { message, type, code: null, param: null } });
}

function isAuthorized(req: IncomingMessage): boolean {
  if (!config.apiKey) return true;
  const bearer = req.headers.authorization;
  if (bearer === `Bearer ${config.apiKey}`) return true;
  return req.headers['x-api-key'] === config.apiKey;
}

async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    size += (chunk as Buffer).length;
    if (size > 32 * 1024 * 1024) throw new BadRequestError('request body too large');
    chunks.push(chunk as Buffer);
  }
  return Buffer.concat(chunks).toString('utf8');
}

/**
 * Pass claude-native model names through; anything else (e.g. a client
 * hard-coded to a gpt-* id) falls back to the configured default so generic
 * OpenAI clients work without reconfiguration.
 */
function resolveModel(requested: string | undefined): string {
  if (!requested) return config.defaultModel;
  const normalized = requested.trim().toLowerCase();
  const aliases = new Set(['sonnet', 'opus', 'haiku', 'fable', ...config.models.map((m) => m.toLowerCase())]);
  if (normalized.startsWith('claude') || aliases.has(normalized)) return requested.trim();
  return config.defaultModel;
}

const EFFORT_LEVELS = new Set(['low', 'medium', 'high', 'xhigh', 'max']);

async function handleChatCompletions(req: IncomingMessage, res: ServerResponse): Promise<void> {
  let body: ChatCompletionRequest;
  try {
    body = JSON.parse(await readBody(req)) as ChatCompletionRequest;
  } catch (err) {
    sendError(res, 400, err instanceof BadRequestError ? err.message : 'request body must be valid JSON');
    return;
  }

  let promptParts: { systemPrompt: string; prompt: string };
  try {
    promptParts = buildPrompt(body.messages);
  } catch (err) {
    sendError(res, 400, err instanceof Error ? err.message : String(err));
    return;
  }

  const model = resolveModel(body.model);
  const effort =
    body.reasoning_effort && EFFORT_LEVELS.has(body.reasoning_effort) ? body.reasoning_effort : undefined;
  const id = newCompletionId();
  const created = Math.floor(Date.now() / 1000);

  const abort = new AbortController();
  req.on('close', () => {
    if (!res.writableEnded) abort.abort();
  });

  await slots.acquire();
  try {
    if (body.stream) {
      res.writeHead(200, {
        'content-type': 'text/event-stream; charset=utf-8',
        'cache-control': 'no-cache',
        connection: 'keep-alive',
        'x-accel-buffering': 'no',
      });
      const write = (data: unknown) => {
        if (!res.writableEnded) res.write(`data: ${JSON.stringify(data)}\n\n`);
      };
      write(streamChunk(id, created, model, { role: 'assistant', content: '' }));

      try {
        const completion = await runClaude({
          claudeBin: config.claudeBin,
          prompt: promptParts.prompt,
          systemPrompt: promptParts.systemPrompt,
          model,
          effort,
          timeoutMs: config.timeoutMs,
          signal: abort.signal,
          onTextDelta: (text) => write(streamChunk(id, created, model, { content: text })),
        });
        write(
          streamChunk(
            id,
            created,
            completion.model,
            {},
            mapFinishReason(completion.stopReason),
            toOpenAIUsage(completion.usage),
          ),
        );
        if (!res.writableEnded) res.write('data: [DONE]\n\n');
      } catch (err) {
        // Headers are already out — surface the failure as a terminal SSE event.
        const message = err instanceof Error ? err.message : String(err);
        write({ error: { message, type: 'api_error', code: null, param: null } });
      } finally {
        res.end();
      }
      return;
    }

    const completion = await runClaude({
      claudeBin: config.claudeBin,
      prompt: promptParts.prompt,
      systemPrompt: promptParts.systemPrompt,
      model,
      effort,
      timeoutMs: config.timeoutMs,
      signal: abort.signal,
    });
    sendJson(res, 200, completionResponse(id, created, completion));
  } catch (err) {
    if (res.headersSent) {
      res.end();
      return;
    }
    if (err instanceof ClaudeCliError) {
      sendError(res, err.statusCode >= 400 && err.statusCode < 600 ? err.statusCode : 502, err.message, 'api_error');
    } else {
      sendError(res, 500, err instanceof Error ? err.message : 'internal error', 'api_error');
    }
  } finally {
    slots.release();
  }
}

const server = createServer((req, res) => {
  const url = new URL(req.url ?? '/', `http://${req.headers.host ?? 'localhost'}`);

  if (req.method === 'GET' && url.pathname === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }

  if (!isAuthorized(req)) {
    sendError(res, 401, 'invalid or missing API key', 'authentication_error');
    return;
  }

  if (req.method === 'GET' && url.pathname === '/v1/models') {
    sendJson(res, 200, {
      object: 'list',
      data: config.models.map((modelId) => ({
        id: modelId,
        object: 'model',
        created: 0,
        owned_by: 'anthropic',
      })),
    });
    return;
  }

  if (req.method === 'POST' && url.pathname === '/v1/chat/completions') {
    void handleChatCompletions(req, res);
    return;
  }

  sendError(res, 404, `no route for ${req.method} ${url.pathname}`, 'not_found_error');
});

server.listen(config.port, config.host, () => {
  console.log(
    `llm-gateway listening on http://${config.host}:${config.port} ` +
      `(default model: ${config.defaultModel}, concurrency: ${config.concurrency}, ` +
      `auth: ${config.apiKey ? 'api key required' : 'open'})`,
  );
});
