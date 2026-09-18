# cmdc2api

English | [简体中文](README_zh.md)

A high-fidelity multi-protocol reverse proxy bridging the Anthropic Messages API, OpenAI Responses API (for Codex CLI), and OpenAI Chat Completions API directly to cmdc (Command Code).

Written in Go with **zero third-party dependencies**, shipped as a single static binary. Downstream clients speaking Anthropic Messages (such as Claude Code and other CLI coding assistants), OpenAI Responses (such as Codex CLI), or OpenAI Chat Completions connect to this proxy. Requests are converted in a **single hop** (star topology with Anthropic Messages as the canonical internal representation) to the cmdc envelope format and forwarded upstream — preserving multi-modal images, thinking blocks, and cache markers (`cache_control`) — while reproducing the official CLI's client fingerprint and behavioral characteristics at the transport layer.

---

## Features

- **Star Topology & Single-hop Conversion** — Anthropic Messages (first-class citizen), OpenAI Responses (`/v1/responses`), and OpenAI Chat Completions (`/v1/chat/completions`) all normalize directly into canonical formats without intermediary translation loss, forwarding to cmdc in a single hop.
- **Deep Codex CLI Compatibility** — Native support for Codex custom tools, grammar definition hints, tool search discovery promotion, namespaced tools flattening/restoration, and strict Responses wire constraints (exact index zero preservation, mandatory arguments keys, array contents, reasoning item C# SDK safety).
- **Robust Tool Pairing & Repair** — Shared `merge → pair → merge` algorithm automatically repairs interleaved tool outputs, drops dangling/unanswered calls, strips orphan results with warnings, and enforces strict user/assistant alternation.
- **Full Multi-modal Support** — User-message images (base64 / URL) pass through natively; images embedded inside `tool_result` blocks are automatically relocated and reconstructed.
- **Smart Cache Affinity** — Native `cache_control` markers preserved; tail cache breakpoints synthesized when missing; combined with stable prefix-derived sessions to maximize Prompt Cache hit rates.
- **High-fidelity Client Masquerade** — Win32 x64 device fingerprint persistence, scheduled lifecycle pre-requests, dynamic npm CLI version synchronization, W3C traceparent headers, and transport-layer alignment (forced HTTP/1.1 with suppressed User-Agent).
- **Streaming & Non-streaming Consistency** — Upstream is always streamed; a unified state machine drives both streaming SSE frames and aggregated JSON for each protocol with identical data semantics.
- **Accurate Billing & Anti-overcharge** — Native `noCacheTokens` prioritized for deduplicated internal accounting; additive backfill for OpenAI prompt tokens (`prompt = noCache + cache_read + cache_creation`); zero-output abnormal responses surface as 429 with `Retry-After`.

---

## Architecture & Workflow

```text
Downstream Clients
 ├─ Claude Code / Anthropic SDK / CLI  ──► POST /v1/messages
 ├─ Codex CLI                          ──► POST /v1/responses
 └─ OpenAI SDK / Compatible Clients    ──► POST /v1/chat/completions
                     │
                     ▼
       ┌────────────────────────────────────────────────────────┐
       │ cmdc2api Proxy (Star Topology)                         │
       │  ├─ 1. Inbound Normalization (chatin / respin / msg)   │
       │  ├─ 2. Tool Pairing & Repair (merge → pair → merge)    │
       │  ├─ 3. Session Affinity (Header / Prefix Hash / Key)   │
       │  ├─ 4. Masquerade Check (Win32 Fingerprint / Version)  │
       │  ├─ 5. Envelope & Tail Cache Synthesis (BuildCcRequest)│
       │  └─ 6. Outbound State Machine (SSE Encoders / Agg)     │
       └───────────────────────────┬────────────────────────────┘
                                   │ Upstream HTTPS (HTTP/1.1, Empty UA, Win32 Headers)
                                   ▼
                     cmdc API (api.commandcode.ai)
```

---

## Quick Start

### Method 1: Docker (Recommended)

Pre-built multi-arch images (`linux/amd64` and `linux/arm64`):

```bash
docker compose up -d   # ghcr.io/b1anyu/cmdc2api:latest
```

Or run directly with Docker:

```bash
docker run -d \
  --name cmdc2api \
  -p 8050:8050 \
  -e HOST=0.0.0.0 \
  -v $(pwd)/data:/app/data \
  ghcr.io/b1anyu/cmdc2api:latest
```

### Method 2: Build from Source

```bash
go build -o cmdc2api . && ./cmdc2api
```

The server listens on `127.0.0.1:8050` by default. Authenticate using your cmdc account API Key (`user_xxx`), which is forwarded to the upstream unchanged.

---

## Client Usage Examples

### 1. Anthropic Messages (`/v1/messages`)

```bash
curl http://127.0.0.1:8050/v1/messages \
  -H "x-api-key: user_xxx" \
  -H "content-type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-flash",
    "max_tokens": 1024,
    "stream": true,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

### 2. Codex CLI (`/v1/responses`)

Configure Codex to point to the proxy via environment variable or config file:

```bash
export CODEX_API_BASE="http://127.0.0.1:8050/v1"
export CODEX_API_KEY="user_xxx"
codex
```

Or in `~/.codex/config.toml`:

```toml
base_url = "http://127.0.0.1:8050/v1"
api_key = "user_xxx"
```

### 3. OpenAI Chat Completions (`/v1/chat/completions`)

```bash
curl http://127.0.0.1:8050/v1/chat/completions \
  -H "Authorization: Bearer user_xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

---

## Supported Endpoints

| Path | Method | Description |
|---|---|---|
| `/v1/messages` | `POST` | Anthropic Messages endpoint (streaming SSE & non-streaming JSON) |
| `/v1/responses` | `POST` | OpenAI Responses endpoint (optimized for Codex CLI, streaming SSE & non-streaming JSON) |
| `/v1/chat/completions` | `POST` | OpenAI Chat Completions endpoint (streaming SSE & non-streaming JSON) |
| `/v1/models` | `GET` | Model list (superset of Anthropic & OpenAI formats; forwarded upstream when authenticated) |
| `/health` | `GET` | Health check endpoint |

**Authentication Headers**:
- Supports `Authorization: Bearer user_xxx` or `x-api-key: user_xxx` headers.
- Account API keys (typically in `user_xxx` format) are passed through to upstream cmdc unchanged without plaintext persistence.

---

## Configuration (Environment Variables)

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8050` | HTTP listening port |
| `HOST` | `127.0.0.1` | HTTP listening address (use `0.0.0.0` in container environments) |
| `CC_API_BASE` | `https://api.commandcode.ai` | cmdc upstream base API URL |
| `CC_STATE_FILE` | `data/state.json` | Persistence path for device fingerprint and lifecycle throttle state |
| `CC_MAX_BODY_MB` | `100` | Inbound request body limit (in MB) |
| `CC_SESSION_STRATEGY` | `prefix` | Session strategy: `prefix` (stable per conversation, recommended) / `key` (per-key, 12h+1h rotation) |
| `CC_CACHE_MARKERS` | `respect` | Cache breakpoint strategy: `respect` (pass client markers through, tail synthesis when absent) / `replace` (strip markers and force tail synthesis for diagnostics) |
| `CC_ASSISTANT_REASONING` | `1` | Replay historical assistant thinking blocks upstream as `{type: "reasoning"}` (`1` to enable, `0` to disable) |
| `CC_FAKE_NODE_VERSION` | `v22.21.0` | Node.js version reported in envelope's `environment` field |
| `CMD_ZDR` | `0` | Attach `x-cmd-zdr: 1` header globally (route via ZDR only; overridable per request) |
| `CC_MODEL_REFRESH` | `5m` | In-memory cache duration for the model list |

---

## Design Details & Mechanics

### 1. Star Topology & Single-hop Conversion Path

The cmdc envelope natively adheres to a messages-centric design (`content` as block arrays, tools defined with `input_schema`, Anthropic-style `tool_choice`). 

cmdc2api establishes a star topology where Anthropic Messages serves as the canonical internal representation:
- `/v1/responses` and `/v1/chat/completions` requests are normalized directly into canonical `types.Request` form **before** `BuildCcRequest`.
- Session affinity, prompt cache breakpoint synthesis, Win32 masquerading, and token usage accounting are shared transparently across all endpoints without code duplication.
- Outbound format differences are resolved strictly after `types.StreamEvent` generation through protocol-specific `EventEncoder` and `EventAggregator` implementations.

### 2. Tool Pairing & Ordering Invariants

Anthropic and cmdc enforce strict invariants on tool execution history:
1. Every `tool_result` must immediately follow the assistant message containing the corresponding `tool_use`.
2. Every `tool_use` must be answered by a subsequent user `tool_result`.
3. User and assistant messages must strictly alternate.

Clients speaking OpenAI formats frequently violate these constraints by emitting interleaved tool results, orphan results, or sibling calls. The proxy's shared `merge → pair → merge` algorithm ([`translate/pairing.go`](internal/translate/pairing.go)) reconstructs correct pairings, drops unanswered calls and orphan results with observability logs, and restores valid alternating order.

### 3. Codex CLI Wire Constraints & Tool Mapping

Codex CLI expects strict conformances to the Responses specification:
- **Wire Format**: SSE events and items are constructed manually to defeat `omitempty` zero-omissions (ensuring indices like `output_index: 0` are preserved). Function call arguments are non-optional keys (`arguments: ""`), messages have array `content: []`, and reasoning items never contain fake IDs or null `status`/`content` fields (preventing C# SDK crashes).
- **Tool Hierarchy**: Codex custom tools (`custom`), tool search proxies (`tool_search`), and namespaced tools (`namespace`) are downgraded to standard functions upon ingress and restored on egress (`fc_`, `ctc_`, `tsc_` prefixes and namespacing).
- **Tool Search Discovery**: Discovered tools in `tool_search_output` items are promoted to full tool declarations dynamically.

### 4. Cache Affinity & Prompt Cache Optimization

cmdc's Prompt Cache operates at session granularity. The proxy orchestrates three tiers of cache affinity:

1. **Explicit Session Headers**: If the request contains `x-session-id`, `x-claude-code-session-id`, or `session_id` (length ≥ 8), it is used directly as the upstream session ID.
2. **Prefix Derivation (Default)**: In the absence of an explicit header, a stable UUID-formatted session ID is calculated from `sha256(system + tools + first_user_message)`. Conversations reuse the exact same upstream session across turns, automatically rotating only when system prompts, toolsets, or compressed contexts change.
3. **Content-level & Tail Cache Breakpoints**:
   - Native `cache_control` annotations on content blocks are preserved (normalized to `{type: "ephemeral"}`).
   - Breakpoints on `system` and `tools` are folded into a synthesized marker on the **very last text part of the envelope** (scanned backwards from the entire conversation tail across tool turns), ensuring cached prefix coverage over all prior history.

### 5. High-fidelity Client Masquerade

Mirrors the observable characteristics of the official Node CLI:
- **Fingerprint Continuity**: Persists generated Windows x64 device components to disk, avoiding anti-abuse triggers caused by frequent restarts.
- **Lifecycle Heartbeats**: Periodically dispatches `/alpha/fingerprint/record` and `/alpha/lifecycle-events` requests on an 8h + 2h jittered schedule.
- **Dynamic CLI Version**: Queries the npm registry every 24 hours to sync the latest release version into `x-command-code-version`.
- **Transport Alignment**: Enforces HTTP/1.1 connections and suppresses the default Go `User-Agent` header to match Node.js `undici` client behavior.

### 6. Accurate Token Accounting

Empirical verification revealed that cmdc upstream's `inputTokens` accumulates cached reads across internal loop steps (approximately 2× console totals). The proxy prioritizes upstream's native `noCacheTokens` when available, falling back to:

```text
input_tokens = max(0, inputTokens - cachedInputTokens)
```

For OpenAI Responses and Chat Completions endpoints (where `prompt_tokens` represents the total prompt size including cache hits), usage is backfilled additively:

```text
prompt_tokens = input_tokens (noCache) + cache_read (+ cache_creation)
cached_tokens = cache_read
total_tokens  = prompt_tokens + output_tokens
```

---

## Known Limitations & Fallbacks

- **System Prompt Format**: cmdc strictly enforces `params.system` as a plain string (array payloads are rejected with HTTP 400). Structured system blocks are concatenated into text, with cache markers mapped to the tail.
- **Thinking Signature Deterministic Forgery**: Because third-party proxies cannot mint official cryptographic signatures, signatures are deterministically generated via `0x12` + sha256 prefix, ensuring stable signatures for identical thinking blocks while passing client-side shallow validation.
- **Unsupported Inbound Fields**: Fields without upstream counterparts are safely dropped with logs (e.g. `stop_sequences`, `top_k`, `metadata.user_id`, and `tool_result.is_error`). Responses and Chat endpoints also reject unsupported features like `previous_response_id` and `n > 1` with explicit 400 errors.

---

## Project Structure & Development

```
internal/
├── config/       # Environment-driven runtime configuration
├── errs/         # Error mapping and classification
├── masq/         # Client masquerading: fingerprint, session store, npm version, upstream client
├── server/       # HTTP server layer: routes, shared pipeline, CORS middleware, handlers (messages, chat, responses, models)
├── translate/    # Protocol translation: inbound normalization (chatin, respin), outbound encoders/aggregators (chatout, respout), pairing repair, stream translator
└── types/        # Protocol data structures (Anthropic, OpenAI Chat, OpenAI Responses, cmdc envelopes & events)
```

Running tests locally:

```bash
go test -race ./...   # Unit & end-to-end tests (with built-in mock upstream)
go vet ./...
```

---

## References & Acknowledgements

- Protocol analysis and client masquerade mechanics inspired by [commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy) (MIT).
- Architecture techniques and streaming state-machine designs referenced from the `apicompat` package of [sub2api](https://github.com/Wei-Shaw/sub2api) (LGPL-3.0).

---

## License

This project is licensed under the [MIT License](LICENSE).

---

## Disclaimer

This project is provided for educational and technical research purposes only. Users are solely responsible for ensuring compliance with applicable laws, regulations, and third-party service terms. The authors assume no liability for any consequences arising from the use of this project.
