# cmdc2api

English | [简体中文](README_zh.md)

A direct-mapping proxy from the Anthropic Messages API to cmdc (Command Code).

Written in Go with **zero third-party dependencies**, shipped as a single static binary. Clients that speak the Anthropic protocol (such as Claude Code and other CLI coding assistants) connect to this proxy, which converts each request in a **single hop** to the cmdc envelope format and forwards it upstream — preserving multi-modal images, thinking blocks, and cache markers (`cache_control`) — while reproducing the official CLI's client fingerprint and behavioral characteristics at the transport layer.

---

## Features

- **Single-hop Conversion** — Anthropic → cmdc in one step, bypassing intermediate OpenAI Chat translations to eliminate schema degradation and data loss.
- **Full Multi-modal Support** — User-message images (base64 / URL) pass through natively; images embedded inside `tool_result` blocks are automatically relocated and reconstructed.
- **Smart Cache Affinity** — Native `cache_control` markers preserved; tail cache breakpoints synthesized when missing; combined with stable prefix-derived sessions to maximize Prompt Cache hit rates.
- **High-fidelity Client Masquerade** — Win32 x64 device fingerprint persistence, scheduled lifecycle pre-requests, dynamic npm CLI version synchronization, W3C traceparent headers, and transport-layer alignment (forced HTTP/1.1 with suppressed User-Agent).
- **Streaming & Non-streaming Consistency** — Upstream is always streamed; an internal pure-function state machine emits Anthropic SSE or aggregated JSON on demand with identical data semantics.
- **Accurate Billing & Anti-overcharge** — Token usage read from upstream totals with deduplicated cache accounting; zero-output abnormal responses surface as 429 with `Retry-After` to protect downstream billing.

---

## Architecture & Workflow

```text
Downstream Client (Claude Code / Anthropic SDK / CLI)
                     │
                     │ POST /v1/messages (Anthropic Messages API)
                     ▼
       ┌────────────────────────────────────────────────────────┐
       │ cmdc2api Proxy                                         │
       │  ├─ 1. Session Affinity (Header / Prefix Hash / Key)   │
       │  ├─ 2. Masquerade Check (Win32 Fingerprint / Version)  │
       │  ├─ 3. Single-hop Mapping (Req & Tail Cache Synthesis) │
       │  └─ 4. Response State Machine (NDJSON → SSE / JSON)    │
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

The server listens on `127.0.0.1:8050` by default. Authenticate using your cmdc account API Key, which is forwarded to the upstream unchanged:

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

---

## Supported Endpoints

| Path | Method | Description |
|---|---|---|
| `/v1/messages` | `POST` | Anthropic Messages endpoint (streaming SSE & non-streaming JSON) |
| `/v1/models` | `GET` | Model list (forwarded upstream when authenticated; built-in fallback otherwise) |
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
| `CC_ASSISTANT_REASONING` | `0` | Experimental: Replay historical thinking blocks upstream as `{type: "reasoning"}` |
| `CC_FAKE_NODE_VERSION` | `v22.21.0` | Node.js version reported in envelope's `environment` field |
| `CMD_ZDR` | `0` | Attach `x-cmd-zdr: 1` header globally (route via ZDR only; overridable per request) |
| `CC_MODEL_REFRESH` | `5m` | In-memory cache duration for the model list |

---

## Design Details & Mechanics

### 1. Single-hop Conversion Path

The cmdc envelope natively adheres to a messages-centric design (`content` as block arrays, tools defined with `input_schema`, Anthropic-style `tool_choice`), making it structurally closest to Anthropic's protocol. cmdc2api performs direct single-hop field mapping without an intermediate OpenAI Chat representation, preventing tool argument corruption and image block loss.

### 2. Cache Affinity & Prompt Cache Optimization

cmdc's Prompt Cache operates at the session granularity. The proxy orchestrates three tiers of cache affinity:

1. **Explicit Session Headers**: If the request contains `x-session-id`, `x-claude-code-session-id`, or `session_id` (length ≥ 8), it is used directly as the upstream session ID.
2. **Prefix Derivation (Default)**: In the absence of an explicit header, a stable UUID-formatted session ID is calculated from `sha256(system + tools + first_user_message)`. Conversations reuse the exact same upstream session across turns, automatically rotating only when system prompts, toolsets, or compressed contexts change.
3. **Content-level & Tail Cache Breakpoints**:
   - Native `cache_control` annotations on content blocks are preserved (normalized to `{type: "ephemeral"}`).
   - Breakpoints on `system` and `tools` are folded into a synthesized marker on the **very last text part of the envelope** (scanned backwards from the entire conversation tail across tool turns), ensuring cached prefix coverage over all prior history.

### 3. High-fidelity Client Masquerade

Mirrors the observable characteristics of the official Node CLI:
- **Fingerprint Continuity**: Persists generated Windows x64 device components to disk, avoiding anti-abuse triggers caused by frequent restarts.
- **Lifecycle Heartbeats**: Periodically dispatches `/alpha/fingerprint/record` and `/alpha/lifecycle-events` requests on an 8h + 2h jittered schedule.
- **Dynamic CLI Version**: Queries the npm registry every 24 hours to sync the latest release version into `x-command-code-version`.
- **Transport Alignment**: Enforces HTTP/1.1 connections and suppresses the default Go `User-Agent` header to match Node.js `undici` client behavior.

### 4. Accurate Token Accounting

Empirical verification revealed that cmdc upstream's `inputTokens` accumulates cached reads across internal loop steps (approximately 2× console totals). The proxy applies the normalized formula:
$$\text{input\_tokens} = \max(0, \text{inputTokens} - \text{cachedInputTokens})$$
This aligns with the actual deduplicated prompt counts reported in the cmdc web console.

---

## Known Limitations & Fallbacks

- **System Prompt Format**: cmdc strictly enforces `params.system` as a plain string (array payloads are rejected with HTTP 400). Structured system blocks are concatenated into text, with cache markers mapped to the tail.
- **Thinking Signature Deterministic Forgery**: Because third-party proxies cannot mint official cryptographic signatures, signatures are deterministically generated via `0x12` + sha256 prefix, ensuring stable signatures for identical thinking blocks while passing client-side shallow validation.
- **Unsupported Inbound Fields**: Fields without upstream counterparts are safely dropped (e.g. `stop_sequences`, `top_k`, `metadata.user_id`, and `tool_result.is_error`).

---

## Project Structure & Development

```
internal/
├── config/       # Environment-driven runtime configuration
├── errs/         # Error types and Anthropic error mapping
├── masq/         # Client masquerading: fingerprint, session store, npm version, upstream client
├── server/       # HTTP server layer: routes, CORS middleware, panic recovery, handlers
├── translate/    # Core protocol conversion: request mapping, SSE streaming state machine, aggregation
└── types/        # Protocol data structures (Anthropic inbound, cmdc envelopes & events)
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
