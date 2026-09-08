# cmdc2api

[English](README.md) | 简体中文

A direct-mapping proxy from the Anthropic Messages API to cmdc (Command Code).

Written in Go with zero third-party dependencies, shipped as a static binary.
Clients that speak the Anthropic protocol (CLI coding assistants and similar
tools) connect to this proxy, which converts each request in a single hop to
the cmdc envelope format and forwards it upstream — preserving images, thinking
blocks, and cache markers — while reproducing the official CLI's client
fingerprint at the request layer.

## Features

- **Single-hop conversion** — Anthropic → cmdc in one step, no OpenAI chat intermediary
- **Full image support** — user-message images (base64 / URL) pass through; images embedded in `tool_result` are automatically relocated
- **Cache affinity** — `cache_control` markers preserved and synthesized, combined with stable session derivation for maximum prompt-cache hits
- **Client masquerade** — device fingerprint, lifecycle pre-requests, live CLI version, OTel traceparent, transport-layer fingerprint alignment
- **Streaming & non-streaming** — the upstream is always streamed; the proxy emits Anthropic SSE or aggregated JSON on demand, with identical semantics in both forms
- **Accurate billing semantics** — usage read from the upstream's final totals; zero-output responses surface as 429 with `Retry-After` to prevent downstream mis-billing

## Quick Start

Docker (recommended; linux/amd64 + arm64):

```bash
docker compose up -d   # ghcr.io/b1anyu/cmdc2api:latest
```

Run from source:

```bash
go build -o cmdc2api . && ./cmdc2api
```

The server listens on `127.0.0.1:8050` by default. Authenticate with your cmdc
account key, which is forwarded to the upstream unchanged:

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

## Endpoints

| Path | Description |
|---|---|
| `POST /v1/messages` | Anthropic Messages (streaming SSE + non-streaming JSON) |
| `GET /v1/models` | Model list (forwarded upstream with a key; built-in fallback otherwise) |
| `GET /health` | Health check |

Auth headers: `Authorization: Bearer user_xxx` or `x-api-key: user_xxx`.

## Configuration (environment variables)

| Variable | Default | Description |
|---|---|---|
| `PORT` / `HOST` | `8050` / `127.0.0.1` | Listen address (containers need `HOST=0.0.0.0`) |
| `CC_API_BASE` | `https://api.commandcode.ai` | Upstream base URL |
| `CC_STATE_FILE` | `data/state.json` | Persistence path for the device fingerprint and lifecycle throttle state |
| `CC_MAX_BODY_MB` | `100` | Inbound request body limit |
| `CC_SESSION_STRATEGY` | `prefix` | Session strategy: `prefix` (stable per conversation) / `key` (per-key, 12h+1h rotation) |
| `CC_CACHE_MARKERS` | `respect` | Cache breakpoints: `respect` (pass client markers through, synthesize at tail when absent) / `replace` (strip and force tail synthesis, diagnostic) |
| `CC_ASSISTANT_REASONING` | `0` | Experimental: replay historical thinking blocks upstream as `{type:reasoning}` |
| `CC_FAKE_NODE_VERSION` | `v22.21.0` | Node version reported in the envelope's `environment` field |
| `CMD_ZDR` | `0` | Attach `x-cmd-zdr: 1` to route via ZDR only (also settable per request) |
| `CC_MODEL_REFRESH` | `5m` | Model list cache duration |

## Design Notes

### Conversion path

The cmdc envelope is already messages-flavored (block-array `content`, tools
with `input_schema`, Anthropic-style `tool_choice`) — closest to the Anthropic
protocol itself. This proxy therefore skips the Anthropic → OpenAI chat
intermediate step entirely and maps request fields in one pass, avoiding the
field loss inherent in a two-stage conversion.

### Cache affinity

cmdc's prompt cache works at session granularity. The proxy combines three
mechanisms:

1. **Explicit session headers win**: inbound `x-session-id` /
   `x-claude-code-session-id` / `session_id` (≥ 8 chars) are used as the
   upstream session ID directly;
2. **Prefix derivation** (default): with no explicit header, a stable
   UUID-shaped session ID is derived from
   `sha256(system + tools + first user message)` — the same conversation keeps
   the same session across turns; when the prefix changes (tool set changes,
   context compaction) the session rotates automatically;
3. **Part-level cache markers**: `cache_control` on content blocks is kept in
   place (normalized to `{type:"ephemeral"}`); markers on system / tools are
   folded into a synthesized marker on the **envelope tail** — the breakpoint
   is placed by scanning back from the very end of the conversation (across
   message roles, including tool rounds), so it advances as the conversation
   grows and the cache covers the full history except the newest turn.
   `CC_CACHE_MARKERS=replace` strips client markers and forces tail synthesis
   (diagnostic).

### Client masquerade

Mirrors the observable behavior of the official CLI: a Windows x64 device
fingerprint (persisted, stable across restarts), pre-requests to
`/alpha/fingerprint/record` and `/alpha/lifecycle-events` (throttled on an
8h + 2h jittered schedule), the CLI version pulled live from npm into
`x-command-code-version`, a per-session derived project slug, and a W3C
traceparent. At the transport layer the proxy forces HTTP/1.1 and sends no
User-Agent, matching the Node CLI's outbound characteristics.

### Known limitations

- cmdc requires `params.system` to be a string (arrays are rejected with 400);
  block structure and cache breakpoints on system / tools go through the
  folding path described above;
- thinking signatures are deterministic forgeries (sha256 + 0x12 prefix): a
  third-party proxy cannot mint real signatures, and this scheme satisfies
  clients' shallow validation with a stable signature per text;
- fields with no upstream equivalent are dropped: `stop_sequences`, `top_k`,
  `metadata.user_id`, and `is_error` on tool results;
- the upstream's `inputTokens` double-counts cache reads (confirmed against
  the upstream console); the proxy converts to Anthropic semantics —
  `input_tokens` excludes the cached bucket.

## References

Protocol knowledge and the client-masquerade layer come from
[commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy) (MIT);
conversion techniques draw on the `apicompat` package of
[sub2api](https://github.com/Wei-Shaw/sub2api) (LGPL-3.0). cmdc2api itself is
original code — no source from either project is included.

## License

[MIT](LICENSE)

## Development

```bash
go test -race ./...   # unit + end-to-end tests (built-in fake upstream)
go vet ./...
```

```
internal/config     Configuration loading (environment variables only)
internal/masq       Client masquerade: fingerprint/sessions/version/lifecycle/upstream client
internal/types      Wire protocol types (Anthropic inbound, cmdc envelope & events, SSE events)
internal/translate  Protocol conversion: request mapping, streaming state machine, aggregation
internal/server     HTTP entrypoint: routing/middleware/error mapping
```

## Disclaimer

This project is provided for learning and technical research purposes only.
Users are responsible for ensuring that their use complies with applicable
laws and regulations in their jurisdiction as well as the terms of any services
involved. All consequences arising from the use of this project are borne by
the user; the authors accept no liability.
