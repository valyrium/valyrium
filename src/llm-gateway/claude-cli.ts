/**
 * Thin runner around `claude -p --output-format stream-json`.
 *
 * Each completion spawns one CLI process with all agentic surface disabled
 * (`--tools ""`, no session persistence, no slash commands) and a caller-owned
 * system prompt, so the process behaves like a plain model call while reusing
 * whatever auth the CLI is already configured with (OAuth login or
 * ANTHROPIC_API_KEY).
 *
 * The prompt is written to stdin rather than passed as an argv entry so long
 * conversation transcripts never hit OS argument-length limits.
 */

import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';

export interface ClaudeUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens: number;
  cacheCreationInputTokens: number;
}

export interface ClaudeCompletion {
  text: string;
  model: string;
  stopReason: string | null;
  usage: ClaudeUsage;
  costUsd: number | null;
}

export interface RunClaudeOptions {
  claudeBin: string;
  prompt: string;
  systemPrompt: string;
  model?: string;
  effort?: string;
  timeoutMs: number;
  signal?: AbortSignal;
  /** Called for each assistant text delta while the model is generating. */
  onTextDelta?: (text: string) => void;
}

export class ClaudeCliError extends Error {
  constructor(message: string, readonly statusCode: number = 502) {
    super(message);
    this.name = 'ClaudeCliError';
  }
}

interface StreamJsonLine {
  type: string;
  subtype?: string;
  message?: {
    model?: string;
    content?: Array<{ type: string; text?: string }>;
  };
  event?: {
    type: string;
    delta?: { type: string; text?: string; stop_reason?: string | null };
  };
  is_error?: boolean;
  result?: string;
  stop_reason?: string | null;
  total_cost_usd?: number;
  usage?: {
    input_tokens?: number;
    output_tokens?: number;
    cache_read_input_tokens?: number;
    cache_creation_input_tokens?: number;
  };
}

export function runClaude(options: RunClaudeOptions): Promise<ClaudeCompletion> {
  const args = [
    '-p',
    '--output-format', 'stream-json',
    '--include-partial-messages',
    '--verbose',
    '--tools', '',
    '--no-session-persistence',
    '--disable-slash-commands',
    '--system-prompt', options.systemPrompt,
  ];
  if (options.model) args.push('--model', options.model);
  if (options.effort) args.push('--effort', options.effort);

  return new Promise<ClaudeCompletion>((resolve, reject) => {
    const child = spawn(options.claudeBin, args, {
      stdio: ['pipe', 'pipe', 'pipe'],
      windowsHide: true,
    });

    let settled = false;
    let sawResult = false;
    let streamedText = '';
    let resultText: string | null = null;
    let model = options.model ?? 'unknown';
    let stopReason: string | null = null;
    let costUsd: number | null = null;
    const usage: ClaudeUsage = {
      inputTokens: 0,
      outputTokens: 0,
      cacheReadInputTokens: 0,
      cacheCreationInputTokens: 0,
    };
    let stderrTail = '';

    const finish = (outcome: { ok: true } | { ok: false; error: Error }) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      options.signal?.removeEventListener('abort', onAbort);
      if (child.exitCode === null) child.kill('SIGKILL');
      if (outcome.ok) {
        resolve({
          // The result line is authoritative; streamed deltas are the fallback
          // if the CLI ever omits it on success.
          text: resultText ?? streamedText,
          model,
          stopReason,
          usage,
          costUsd,
        });
      } else {
        reject(outcome.error);
      }
    };

    const onAbort = () => finish({ ok: false, error: new ClaudeCliError('request aborted by client', 499) });
    options.signal?.addEventListener('abort', onAbort, { once: true });
    if (options.signal?.aborted) onAbort();

    const timer = setTimeout(
      () => finish({ ok: false, error: new ClaudeCliError(`claude CLI timed out after ${options.timeoutMs}ms`, 504) }),
      options.timeoutMs,
    );

    child.on('error', (err) => {
      finish({ ok: false, error: new ClaudeCliError(`failed to spawn claude CLI: ${err.message}`, 500) });
    });

    child.stderr.on('data', (chunk: Buffer) => {
      stderrTail = (stderrTail + chunk.toString()).slice(-2000);
    });

    const rl = createInterface({ input: child.stdout });
    rl.on('line', (line) => {
      if (settled || !line.trim()) return;
      let parsed: StreamJsonLine;
      try {
        parsed = JSON.parse(line) as StreamJsonLine;
      } catch {
        return; // non-JSON noise on stdout — ignore
      }

      if (parsed.type === 'stream_event' && parsed.event) {
        const { event } = parsed;
        if (event.type === 'content_block_delta' && event.delta?.type === 'text_delta' && event.delta.text) {
          streamedText += event.delta.text;
          options.onTextDelta?.(event.delta.text);
        } else if (event.type === 'message_delta' && event.delta?.stop_reason) {
          stopReason = event.delta.stop_reason;
        }
        return;
      }

      if (parsed.type === 'assistant' && parsed.message?.model) {
        model = parsed.message.model;
        return;
      }

      if (parsed.type === 'result') {
        sawResult = true;
        if (parsed.is_error) {
          const detail = parsed.result || parsed.subtype || 'unknown error';
          finish({ ok: false, error: new ClaudeCliError(`claude CLI reported an error: ${detail}`) });
          return;
        }
        if (typeof parsed.result === 'string') resultText = parsed.result;
        if (parsed.stop_reason !== undefined) stopReason = parsed.stop_reason;
        if (typeof parsed.total_cost_usd === 'number') costUsd = parsed.total_cost_usd;
        if (parsed.usage) {
          usage.inputTokens = parsed.usage.input_tokens ?? 0;
          usage.outputTokens = parsed.usage.output_tokens ?? 0;
          usage.cacheReadInputTokens = parsed.usage.cache_read_input_tokens ?? 0;
          usage.cacheCreationInputTokens = parsed.usage.cache_creation_input_tokens ?? 0;
        }
      }
    });

    child.on('close', (code) => {
      if (settled) return;
      if (sawResult) {
        finish({ ok: true });
      } else {
        const detail = stderrTail.trim() || `exit code ${code}`;
        finish({ ok: false, error: new ClaudeCliError(`claude CLI exited without a result: ${detail}`) });
      }
    });

    child.stdin.write(options.prompt);
    child.stdin.end();
  });
}
