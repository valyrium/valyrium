# llm-gateway

A small HTTP service that exposes a **standard OpenAI-compatible LLM API** and internally routes every request to the **Claude Code CLI** (`claude -p`). Any tool that speaks the OpenAI Chat Completions protocol — the `openai` SDKs, Open WebUI, LiteLLM, LangChain, curl — can talk to Claude models through whatever auth your local `claude` is already logged in with.

```
OpenAI client ──HTTP──▶ llm-gateway ──spawn──▶ claude -p --output-format stream-json ──▶ Claude
```

Zero runtime dependencies: plain `node:http` plus a `claude` binary on your PATH.

## Run

```bash
npm run llm-gateway
# or directly:
bun src/llm-gateway/server.ts
```

Then point any OpenAI client at it:

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model": "sonnet", "messages": [{"role": "user", "content": "Say hi"}]}'
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="unused")
resp = client.chat.completions.create(
    model="sonnet",
    messages=[{"role": "user", "content": "Say hi"}],
    stream=True,
)
for chunk in resp:
    print(chunk.choices[0].delta.content or "", end="")
```

## Endpoints

| Route | Behavior |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions; supports `stream: true` (SSE) and non-streaming. `usage` is populated from the CLI's token accounting. |
| `GET /v1/models` | Lists the models from `CLAUDE_GATEWAY_MODELS`. |
| `GET /healthz` | Liveness probe (never requires auth). |

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `CLAUDE_GATEWAY_PORT` | `8787` | Listen port |
| `CLAUDE_GATEWAY_HOST` | `127.0.0.1` | Bind address (set `0.0.0.0` to expose on the network) |
| `CLAUDE_GATEWAY_API_KEY` | *(unset)* | When set, requests must send it as `Authorization: Bearer …` or `x-api-key` |
| `CLAUDE_GATEWAY_MODEL` | `sonnet` | Default model, also used when a client requests a non-Claude model id (e.g. `gpt-4o`) |
| `CLAUDE_GATEWAY_MODELS` | `sonnet,opus,haiku` | Model ids advertised by `/v1/models` |
| `CLAUDE_GATEWAY_BIN` | `claude` | Path to the Claude Code CLI |
| `CLAUDE_GATEWAY_TIMEOUT_MS` | `300000` | Per-request CLI timeout |
| `CLAUDE_GATEWAY_CONCURRENCY` | `4` | Max simultaneous CLI processes; excess requests queue |

## Request mapping

- `messages` with `system`/`developer` roles become the CLI's `--system-prompt` (replacing Claude Code's own agent prompt, so responses behave like a plain model).
- A single user message is passed through as-is; multi-turn histories are flattened into a `[user]:` / `[assistant]:` transcript with a continue-the-conversation instruction, because each CLI spawn is stateless.
- `model` accepts Claude aliases (`sonnet`, `opus`, `haiku`, `fable`) or full `claude-*` ids; anything else falls back to the default model.
- `reasoning_effort` (`low`–`max`) maps to the CLI's `--effort`.
- `temperature`, `top_p`, `max_tokens` are accepted but ignored — the CLI does not expose them.
- Every spawn runs with `--tools ""`, `--no-session-persistence`, and `--disable-slash-commands`, so no agentic tools ever execute on the gateway host.

## Limitations and caveats

- **Latency**: each request pays a CLI process startup (~1–2 s) before first token. Fine for chat UIs and batch jobs; not for latency-critical serving.
- **Text only**: image/audio content parts are rejected with a 400.
- **No tool calling passthrough**: OpenAI `tools`/function-calling requests are not translated.
- **Terms of use**: if your `claude` CLI is authenticated with a consumer subscription (OAuth login), Anthropic's consumer terms govern that usage — treat this as a personal/dev convenience, not a way to resell subscription access as an API. With `ANTHROPIC_API_KEY` auth it is simply another API client.
