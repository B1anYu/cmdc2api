# cmdc2api

[English](README.md) | 简体中文

多协议入站反向代理——将 Anthropic Messages API、OpenAI Responses API（面向 Codex CLI）以及 OpenAI Chat Completions API 单跳直转至 cmdc（Command Code）。

基于 Go 语言实现，**零外部第三方依赖**，编译为单一静态二进制文件。下游客户端（如 Claude Code 等各类基于 Anthropic Messages 协议的 CLI 助手、Codex CLI 以及标准 OpenAI 客户端）接入本代理后，请求将以**单跳直转**（星形拓扑，Anthropic Messages 形状作为内部规范格式）转换为 cmdc 信封格式转发至上游——完整保留图片多模态、思考链（thinking blocks）与缓存标记（cache_control），并在请求与传输层高保真还原官方 CLI 的设备指纹与行为特征。

---

## 特性

- **星形拓扑单跳直转（Star Topology & Single-hop Conversion）** —— Anthropic Messages（一等公民）、OpenAI Responses（`/v1/responses`）与 OpenAI Chat Completions（`/v1/chat/completions`）三端点入站后直接归一化为内部规范格式，零二次中转丢失，一步直达 cmdc 信封。
- **Codex CLI 深度兼容** —— 原生支持 Codex custom 自由文本工具与文法定义提示、tool_search 代理工具与发现结果动态提升、namespace 命名空间工具摊平与出站还原；严格对齐 Responses SSE 线格式约束（反 `omitempty` 保留 `0` 索引、参数键恒存在、消息内容恒为数组、reasoning 项绝不带伪造 ID 且剔除 null 字段以防御 C# SDK 崩溃）。
- **双入站共享工具配对修复（Tool Pairing Invariants）** —— 双入站共享 `merge → pair → merge` 算法，自动修复混乱交错的工具结果、重排使其紧随调用、自动清洗悬空调用与孤儿结果并留痕，确保角色严格交替。
- **完整多模态支持** —— 用户消息图片（base64 / URL）原生透传；`tool_result` 中内嵌的图片块自动重构搬迁。
- **智能缓存亲和（Cache Affinity）** —— 保留客户端原生 `cache_control`，缺失时自动在信封尾部合成断点；结合前缀哈希派生会话，最大化 Prompt Cache 命中率。
- **高保真客户端伪装（Client Masquerade）** —— Win32 x64 设备指纹持久化、周期性 lifecycle 预请求、实时从 npm 同步 CLI 版本号、W3C traceparent 追踪头、传输层强制 HTTP/1.1 并置空 User-Agent。
- **流式 / 非流式双模态一致** —— 上游恒定采用流式传输，内部统一状态机按需驱动各协议 SSE 流分片或折叠为完整 JSON 响应，流式与非流式语义完全等价。
- **准确计费与防误扣** —— 内部规范优先直读上游原生 `noCacheTokens` 去重口径；OpenAI 出站协议统一加法回填总量（`prompt = noCache + cache_read + cache_creation`）；上游零输出异常响应转换为 429 并附带 `Retry-After`。

---

## 架构与流程

```text
Downstream Clients
 ├─ Claude Code / Anthropic SDK / CLI  ──► POST /v1/messages
 ├─ Codex CLI                          ──► POST /v1/responses
 └─ OpenAI SDK / Compatible Clients    ──► POST /v1/chat/completions
                     │
                     ▼
       ┌────────────────────────────────────────────────────────┐
       │ cmdc2api Proxy（星形拓扑）                              │
       │  ├─ 1. 入站归一化（chatin / respin / messages）        │
       │  ├─ 2. 配对修复（merge → pair → merge 严格交替）       │
       │  ├─ 3. 会话亲和解析（Header / 前缀哈希 / Key 轮换）     │
       │  ├─ 4. 伪装状态检查（Win32 指纹 / npm 版本 / 预请求）  │
       │  ├─ 5. 信封构造与尾部断点合成（BuildCcRequest）        │
       │  └─ 6. 出站状态机驱动（SSE Encoders / Aggregators）    │
       └───────────────────────────┬────────────────────────────┘
                                   │ Upstream HTTPS (HTTP/1.1, 空 UA, Win32 Headers)
                                   ▼
                     cmdc API (api.commandcode.ai)
```

---

## 快速开始

### 方式一：Docker 部署（推荐）

支持 `linux/amd64` 与 `linux/arm64` 双架构：

```bash
docker compose up -d   # 使用 ghcr.io/b1anyu/cmdc2api:latest
```

或直接运行容器：

```bash
docker run -d \
  --name cmdc2api \
  -p 8050:8050 \
  -e HOST=0.0.0.0 \
  -v $(pwd)/data:/app/data \
  ghcr.io/b1anyu/cmdc2api:latest
```

### 方式二：本地源码编译

```bash
go build -o cmdc2api . && ./cmdc2api
```

服务默认监听 `127.0.0.1:8050`。鉴权直接使用你的 cmdc 账号 API Key（通常为 `user_xxx` 格式），代理将原样透传至上游。

---

## 客户端接入示例

### 1. Anthropic Messages 端点（`/v1/messages`）

适用于 Claude Code 及支持 Anthropic 协议的工具：

```bash
curl http://127.0.0.1:8050/v1/messages \
  -H "x-api-key: user_xxx" \
  -H "content-type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-flash",
    "max_tokens": 1024,
    "stream": true,
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

### 2. Codex CLI（`/v1/responses`）

配置环境变量将 Codex 指向代理：

```bash
export CODEX_API_BASE="http://127.0.0.1:8050/v1"
export CODEX_API_KEY="user_xxx"
codex
```

或在 `~/.codex/config.toml` 中配置：

```toml
base_url = "http://127.0.0.1:8050/v1"
api_key = "user_xxx"
```

### 3. OpenAI Chat Completions 端点（`/v1/chat/completions`）

适用于通用 OpenAI 兼容客户端与各类应用：

```bash
curl http://127.0.0.1:8050/v1/chat/completions \
  -H "Authorization: Bearer user_xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

---

## 支持端点

| 路径 | 方法 | 说明 |
|---|---|---|
| `/v1/messages` | `POST` | Anthropic Messages 接口（支持流式 SSE 与非流式 JSON） |
| `/v1/responses` | `POST` | OpenAI Responses 接口（面向 Codex CLI 深度适配，支持流式 SSE 与非流式 JSON） |
| `/v1/chat/completions` | `POST` | OpenAI Chat Completions 接口（标准对话补全，支持流式 SSE 与非流式 JSON） |
| `/v1/models` | `GET` | 模型列表（Anthropic 与 OpenAI 格式超集；携带 Key 时透传上游，否则回退至内置列表） |
| `/health` | `GET` | 健康检查端点 |

**鉴权头**：
- 支持 `Authorization: Bearer user_xxx` 或 `x-api-key: user_xxx` 请求头。
- 账号 Key（通常为 `user_xxx` 格式）原样透传至上游 cmdc，代理本身不校验合法性，且绝不持久化明文。

---

## 配置说明（环境变量）

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `8050` | HTTP 监听端口 |
| `HOST` | `127.0.0.1` | HTTP 监听地址（容器部署时需设置为 `0.0.0.0`） |
| `CC_API_BASE` | `https://api.commandcode.ai` | cmdc 上游 API 基地址 |
| `CC_STATE_FILE` | `data/state.json` | 设备指纹与生命周期预请求节流状态的持久化文件路径 |
| `CC_MAX_BODY_MB` | `100` | 入站请求体大小上限（单位：MB） |
| `CC_SESSION_STRATEGY` | `prefix` | 会话策略：`prefix`（按对话前缀稳定派生，推荐）/ `key`（按 Key 12h+1h 抖动轮换） |
| `CC_CACHE_MARKERS` | `respect` | 缓存断点策略：`respect`（透传客户端标记，缺失时末尾合成兜底）/ `replace`（剥除客户端标记并强制末尾合成，用于诊断） |
| `CC_ASSISTANT_REASONING` | `1` | 历史思考块回传策略：将历史 assistant 思考块以 `{type: "reasoning"}` 回传上游（`1` 开启，`0` 禁用） |
| `CC_FAKE_NODE_VERSION` | `v22.21.0` | 信封 `environment` 字段中上报的伪装 Node.js 版本 |
| `CMD_ZDR` | `0` | 全局附加 `x-cmd-zdr: 1` 请求头（仅走 ZDR 路由，也可在单个请求头中覆盖） |
| `CC_MODEL_REFRESH` | `5m` | 模型列表的内存缓存刷新间隔 |

---

## 设计细节与核心机制

### 1. 星形拓扑与单跳直转路径

cmdc 信封自身采用 messages 体系设计（`content` 均为块数组、工具定义使用 `input_schema`、`tool_choice` 为 Anthropic 风格）。

cmdc2api 采用星形拓扑，将 Anthropic Messages 形状作为内部唯一规范格式：
- `/v1/responses` 与 `/v1/chat/completions` 入站请求在进入 `BuildCcRequest` 前直接完成向规范请求（[`types.Request`](internal/types/anthropic.go)）的归一化。
- 会话亲和计算、缓存断点合成、Win32 伪装环境与计费转换在管线内完全复用，避免多套实现分化。
- 出站协议差异全部收敛在 `types.StreamEvent` 产出之后，由各自协议专属的 `EventEncoder` 与 `EventAggregator` 分离处理。

### 2. 工具调用配对修复与顺序不变式

Anthropic 与 cmdc 上游对消息历史有三条强约束：
1. 每个 `tool_result` 必须紧邻包含对应 `tool_use` 的 assistant 消息；
2. 每个 `tool_use` 必须被后续 user 的 `tool_result` 明确应答；
3. user 与 assistant 必须严格交替。

OpenAI 风格客户端常会在 call 与 output 之间插入并行调用结果或审批提示，破坏上述约束。代理内部的共享 `merge → pair → merge` 算法（[`translate/pairing.go`](internal/translate/pairing.go)）会自动重建配对，剔除悬空调用与孤儿结果并记录可观测告警日志，确保上游永远接收合法序列。

### 3. Codex CLI 出站硬约束与工具族处理

Codex CLI 对 Responses 规范的校验极度严格：
- **线格式细节**：SSE 帧反 `omitempty` 逐事件手动构造，保证 `output_index` / `content_index` 的 `0` 不被吞掉；函数参数键强制出现（`arguments: ""`）；内容项恒为数组（`content: []`）；reasoning 项绝不伪造 ID 且剔除 null 字段（避免 C# SDK 解析崩溃）。
- **工具族降级与还原**：Codex 专有的 custom 自由文本工具、tool_search 代码搜索工具和 namespace 命名空间工具在入站时降级为标准函数，并在出站时依映射表（[`ResponsesToolMapping`](internal/translate/responses_mapping.go)）还原为 `fc_`、`ctc_`、`tsc_` ID 前缀与对应 item 形态。
- **发现提升**：`tool_search_output` 中的搜索发现工具会被动态提升为正式工具声明并追加至上下文末尾。

### 4. 会话亲和与缓存优化（Prompt Cache）

cmdc 上游的 Prompt Cache 依赖会话（Session）粒度运作。代理结合三层机制实现高命中率：

1. **显式会话头优先**：若请求头包含 `x-session-id`、`x-claude-code-session-id` 或 `session_id`（长度 ≥ 8），直接作为上游会话 ID。
2. **前缀哈希派生（默认）**：无显式头时，由 `sha256(system + tools + 首条用户消息)` 计算生成稳定 UUID 格式的会话 ID。同一轮对话跨轮次天然复用同一会话；当系统提示词、工具集或初始上下文发生压缩/变更时自动迁移。
3. **内容级与末尾缓存断点**：
   - 消息内容块上的 `cache_control` 原位保留（归一化为 `{type: "ephemeral"}`）。
   - `system` 与 `tools` 上的缓存标记折算为在**信封最末尾**的最后一个文本块上合成标记（从整条对话历史末尾向前回扫，跨越工具交互轮次），确保缓存能够覆盖除最新一轮外的所有历史上下文。

### 5. 客户端拟真伪装

全面复刻官方 Node CLI 的可观察行为：
- **指纹连续性**：生成固定的 Windows x64 设备指纹并持久化落盘，避免因重启频繁更换指纹触发风控。
- **预请求与节流**：按 8h + 2h 抖动周期向 `/alpha/fingerprint/record` 与 `/alpha/lifecycle-events` 发送心跳预请求。
- **动态 CLI 版本**：每 24 小时从 npm registry 获取官方最新版本写入 `x-command-code-version`。
- **传输层一致性**：强制出站连接走 `HTTP/1.1` 并置空 `User-Agent`（对齐 Node.js `undici` 客户端的默认网络行为）。

### 6. 准确的 Token 计费与回填

上游返回的 `inputTokens` 在内部多步循环中按步累加 usage。代理内部优先读取上游原生 `noCacheTokens`，缺失时执行减法兜底：

```text
input_tokens = max(0, inputTokens - cachedInputTokens)
```

对于 OpenAI Responses 与 Chat Completions 出站端点（其协议语义中 prompt tokens 为**含缓存总量**），由统一转换函数执行加法回填：

```text
prompt_tokens = input_tokens (noCache) + cache_read (+ cache_creation)
cached_tokens = cache_read
total_tokens  = prompt_tokens + output_tokens
```

---

## 已知限制与降级处理

- **系统提示词单字符串限制**：cmdc 强制要求 `params.system` 为纯字符串（传入数组会被上游直接拒收报错 400）。代理会自动将多段 system/developer 块拼装为纯文本，其上的缓存标记按规则迁移至信封尾部。
- **思考签名确定性伪造与密文往返**：第三方反代无法获得上游私钥签发真实签名，代理采用确定性伪造方案（`0x12` + sha256 前缀），满足下游客户端的浅校验。Responses 出站时作为 `encrypted_content` 下发供 Codex CLI 回放，入站时安全还原思考明文并剥离签名，闭环自洽。
- **不支持字段丢弃与拒绝**：上游无对应能力的字段会被安全忽略并留痕（如 `stop_sequences`、`top_k`、`metadata.user_id` 以及 `tool_result` 的 `is_error` 等）。Responses 路径对 `previous_response_id` 显式返回 400（本代理无服务端会话持久化），Chat Completions 对 `n > 1` 显式返回 400。

---

## 源码结构与开发

```
internal/
├── config/       # 运行配置加载（环境变量驱动，安全默认值）
├── errs/         # 错误类型分类与 HTTP 状态映射
├── masq/         # 客户端伪装：指纹生成/持久化、会话管理、npm 版本同步、上游请求客户端
├── server/       # HTTP 服务层：路由分发、共享管线（pipeline）、CORS 中间件、各协议端点处理
├── translate/    # 协议转换核心：入站归一化（chatin/respin）、配对修复（pairing）、出站编码/聚合（chatout/respout）、流式状态机
└── types/        # 协议数据结构定义（Anthropic、OpenAI Chat、OpenAI Responses、cmdc 信封与事件）
```

本地运行测试：

```bash
go test -race ./...   # 运行单元测试与集成测试（内置 Mock 上游）
go vet ./...
```

---

## 参考与致谢

- 协议分析与客户端伪装机制参考自 [commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy) (MIT)。
- 协议转换架构与状态机思路参考自 [sub2api](https://github.com/Wei-Shaw/sub2api) (LGPL-3.0) 的 `apicompat` 包。

---

## 许可证

本项目遵循 [MIT License](LICENSE) 开源协议。

---

## 免责声明

本项目仅供技术学习与研究使用。使用者应自行确保使用行为符合当地法律法规及相关服务的使用条款。因使用本项目产生的任何后果均由使用者自行承担，项目作者不承担任何责任。
