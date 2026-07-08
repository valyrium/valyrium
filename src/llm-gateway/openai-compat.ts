/**
 * Translation between the OpenAI Chat Completions wire format and the
 * single-prompt shape the Claude Code CLI accepts.
 *
 * The CLI is stateless per spawn and takes one prompt string, so multi-turn
 * conversations are flattened into a transcript with an explicit
 * continue-the-conversation instruction appended to the system prompt. A
 * single-user-message request passes through untouched.
 */

import type { ClaudeCompletion, ClaudeUsage } from './claude-cli.js';

export interface OpenAIContentPart {
  type: string;
  text?: string;
}

export interface OpenAIMessage {
  role: 'system' | 'developer' | 'user' | 'assistant' | 'tool';
  content: string | OpenAIContentPart[] | null;
}

export interface ChatCompletionRequest {
  model?: string;
  messages: OpenAIMessage[];
  stream?: boolean;
  reasoning_effort?: string;
  // Accepted for client compatibility but not forwardable through the CLI:
  temperature?: number;
  top_p?: number;
  max_tokens?: number;
  max_completion_tokens?: number;
}

export class BadRequestError extends Error {
  readonly statusCode = 400;
}

const DEFAULT_SYSTEM_PROMPT = 'You are a helpful assistant.';

const TRANSCRIPT_INSTRUCTION =
  'The user message contains a conversation transcript with turns marked ' +
  '"[user]:" and "[assistant]:". Continue the conversation by writing the next ' +
  'assistant reply to the final user turn. Output only the reply itself, with ' +
  'no role marker or commentary about the transcript.';

function textOf(message: OpenAIMessage): string {
  if (message.content == null) return '';
  if (typeof message.content === 'string') return message.content;
  const parts = message.content.filter((part) => part.type === 'text' && typeof part.text === 'string');
  if (parts.length < message.content.length) {
    throw new BadRequestError('only text content parts are supported by this gateway');
  }
  return parts.map((part) => part.text).join('');
}

export function buildPrompt(messages: OpenAIMessage[]): { systemPrompt: string; prompt: string } {
  if (!Array.isArray(messages) || messages.length === 0) {
    throw new BadRequestError('messages must be a non-empty array');
  }

  const systemParts: string[] = [];
  const turns: Array<{ role: 'user' | 'assistant'; text: string }> = [];

  for (const message of messages) {
    switch (message.role) {
      case 'system':
      case 'developer':
        systemParts.push(textOf(message));
        break;
      case 'user':
        turns.push({ role: 'user', text: textOf(message) });
        break;
      case 'assistant':
        turns.push({ role: 'assistant', text: textOf(message) });
        break;
      default:
        throw new BadRequestError(`unsupported message role: ${message.role}`);
    }
  }

  if (turns.length === 0 || turns[turns.length - 1].role !== 'user') {
    throw new BadRequestError('the last non-system message must be a user message');
  }

  let systemPrompt = systemParts.join('\n\n') || DEFAULT_SYSTEM_PROMPT;

  if (turns.length === 1) {
    return { systemPrompt, prompt: turns[0].text };
  }

  systemPrompt += `\n\n${TRANSCRIPT_INSTRUCTION}`;
  const prompt = turns.map((turn) => `[${turn.role}]: ${turn.text}`).join('\n\n');
  return { systemPrompt, prompt };
}

export function mapFinishReason(stopReason: string | null): string {
  switch (stopReason) {
    case 'max_tokens':
      return 'length';
    case 'refusal':
      return 'content_filter';
    default:
      return 'stop';
  }
}

export function toOpenAIUsage(usage: ClaudeUsage): {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
} {
  // OpenAI clients expect prompt_tokens to cover the whole input; Claude
  // splits it across fresh, cache-read, and cache-write buckets.
  const promptTokens = usage.inputTokens + usage.cacheReadInputTokens + usage.cacheCreationInputTokens;
  return {
    prompt_tokens: promptTokens,
    completion_tokens: usage.outputTokens,
    total_tokens: promptTokens + usage.outputTokens,
  };
}

export function newCompletionId(): string {
  return `chatcmpl-${crypto.randomUUID().replaceAll('-', '')}`;
}

export function completionResponse(id: string, created: number, completion: ClaudeCompletion) {
  return {
    id,
    object: 'chat.completion',
    created,
    model: completion.model,
    choices: [
      {
        index: 0,
        message: { role: 'assistant', content: completion.text },
        finish_reason: mapFinishReason(completion.stopReason),
        logprobs: null,
      },
    ],
    usage: toOpenAIUsage(completion.usage),
  };
}

export function streamChunk(
  id: string,
  created: number,
  model: string,
  delta: Record<string, unknown>,
  finishReason: string | null = null,
  usage: ReturnType<typeof toOpenAIUsage> | null = null,
) {
  const chunk: Record<string, unknown> = {
    id,
    object: 'chat.completion.chunk',
    created,
    model,
    choices: [{ index: 0, delta, finish_reason: finishReason, logprobs: null }],
  };
  if (usage) chunk.usage = usage;
  return chunk;
}
