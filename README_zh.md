# cmdc2api

[English](README.md) | 简体中文

Anthropic Messages API → cmdc（Command Code）直转代理。

基于 Go 语言实现，**零外部第三方依赖**，编译为静态单一二进制文件。客户端（如 Claude Code 等各类基于 Anthropic Messages 协议的 CLI 编程助手与工具）以标准 Anthropic 格式接入，本代理将其请求**单跳直转**为 cmdc 信封格式转发上游，完整保留图片多模态、思考链（thinking blocks）与缓存标记（cache_control），并在请求与传输层高保真还原官方 CLI 的设备指纹与行为特征。

---

## 特性

- **单跳直转（Single-hop Conversion）** —— Anthropic → cmdc 一步到位，跳过 OpenAI Chat 中间层，规避两阶段转换带来的字段丢失与语义失真。
- **完整多模态支持** —— 用户消息图片（base64 / URL）原生透传；`tool_result` 中内嵌的图片块自动重构搬迁。
- **智能缓存亲和（Cache Affinity）** —— 保留客户端原生 `cache_control`，缺失时自动在信封尾部合成断点；结合前缀哈希派生会话，最大化 Prompt Cache 命中率。
- **高保真客户端伪装（Client Masquerade）** —— Win32 x64 设备指纹持久化、周期性 lifecycle 预请求、实时从 npm 同步 CLI 版本号、W3C traceparent 追踪头、传输层强制 HTTP/1.1 并置空 User-Agent。
- **流式 / 非流式双模态一致** —— 上游恒定采用流式传输，代理内部纯函数状态机按需输出 Anthropic SSE 流或聚合成完整 JSON 响应，两种形态语义完全等价。
- **准确计费与防误扣** —— 用量直读上游终值并自动对齐去重口径；上游零输出异常响应转换为 429 并附带 `Retry-After`，防止下游客户端误计费。

---

## 架构与流程

```text
Downstream Client (Claude Code / Anthropic SDK / CLI)
                     │
                     │ POST /v1/messages (Anthropic Messages API)
                     ▼
       ┌────────────────────────────────────────────────────────┐
       │ cmdc2api Proxy                                         │
       │  ├─ 1. 会话亲和解析（Header / 前缀哈希 / Key 轮换）     │
       │  ├─ 2. 伪装状态检查（Win32 指纹 / npm 版本 / 预请求）  │
       │  ├─ 3. 协议单跳直转（Req Mapping & 缓存断点合成）      │
       │  └─ 4. 响应状态机（NDJSON → Anthropic SSE / JSON 聚合）│
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

服务默认监听 `127.0.0.1:8050`。鉴权直接使用你的 cmdc 账号 API Key，代理将原样透传至上游：

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

---

## 支持端点

| 路径 | 方法 | 说明 |
|---|---|---|
| `/v1/messages` | `POST` | Anthropic Messages 接口（支持流式 SSE 与非流式 JSON） |
| `/v1/models` | `GET` | 模型列表（携带 Key 时透传上游获取最新列表，否则回退至内置列表） |
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
| `CC_ASSISTANT_REASONING` | `0` | 实验性开关：将历史思考块以 `{type: "reasoning"}` 形式回传上游 |
| `CC_FAKE_NODE_VERSION` | `v22.21.0` | 信封 `environment` 字段中上报的伪装 Node.js 版本 |
| `CMD_ZDR` | `0` | 全局附加 `x-cmd-zdr: 1` 请求头（仅走 ZDR 路由，也可在单个请求头中覆盖） |
| `CC_MODEL_REFRESH` | `5m` | 模型列表的内存缓存刷新间隔 |

---

## 设计细节与核心机制

### 1. 单跳直转路径

cmdc 信封自身采用 messages 体系设计（`content` 均为块数组、工具定义使用 `input_schema`、`tool_choice` 为 Anthropic 风格），与 Anthropic 协议最为接近。本代理直接进行单跳字段对齐转换，避免了引入 OpenAI Chat 中转所导致的图片格式丢失、工具调用参数失真等问题。

### 2. 会话亲和与缓存优化（Prompt Cache）

cmdc 上游的 Prompt Cache 依赖会话（Session）粒度运作。代理结合三层机制实现高命中率：

1. **显式会话头优先**：若请求头包含 `x-session-id`、`x-claude-code-session-id` 或 `session_id`（长度 ≥ 8），直接作为上游会话 ID。
2. **前缀哈希派生（默认）**：无显式头时，由 `sha256(system + tools + 首条用户消息)` 计算生成稳定 UUID 格式的会话 ID。同一轮对话跨轮次天然复用同一会话；当系统提示词、工具集或初始上下文发生压缩/变更时自动迁移。
3. **内容级与末尾缓存断点**：
   - 消息内容块上的 `cache_control` 原位保留（归一化为 `{type: "ephemeral"}`）。
   - `system` 与 `tools` 上的缓存标记折算为在**信封最末尾**的最后一个文本块上合成标记（从整条对话历史末尾向前回扫，跨越工具交互轮次），确保缓存能够覆盖除最新一轮外的所有历史上下文。

### 3. 客户端拟真伪装

全面复刻官方 Node CLI 的可观察行为：
- **指纹连续性**：生成固定的 Windows x64 设备指纹并持久化落盘，避免因重启频繁更换指纹触发风控。
- **预请求与节流**：按 8h + 2h 抖动周期向 `/alpha/fingerprint/record` 与 `/alpha/lifecycle-events` 发送心跳预请求。
- **动态 CLI 版本**：每 24 小时从 npm registry 获取官方最新版本写入 `x-command-code-version`。
- **传输层一致性**：强制出站连接走 `HTTP/1.1` 并置空 `User-Agent`（对齐 Node.js `undici` 客户端的默认网络行为）。

### 4. 准确的 Token 计费口径

实测发现上游返回的 `inputTokens` 在其内部多步循环中把缓存读取重复累加（数值约为后台控制台统计的两倍）。代理采用换算公式：

```text
input_tokens = max(0, inputTokens - cachedInputTokens)
```

该口径精准对齐 cmdc 后台控制台的去重实际输入统计。

---

## 已知限制与降级处理

- **系统提示词单字符串限制**：cmdc 强制要求 `params.system` 为纯字符串（传入数组会上报 400 错误）。代理会自动将结构化 system 块拼装为纯文本，其上的缓存标记按规则迁移至信封尾部。
- **思考签名确定性伪造**：第三方反代无法获得上游私钥签发真实签名，代理采用确定性伪造方案（`0x12` + sha256 前缀），满足下游客户端的浅校验且同文本签名稳定。
- **不支持字段丢弃**：上游无对应能力的字段会被安全忽略（如 `stop_sequences`、`top_k`、`metadata.user_id` 以及 `tool_result` 的 `is_error`）。

---

## 源码结构与开发

```
internal/
├── config/       # 运行配置加载（环境变量驱动）
├── errs/         # 错误类型与 HTTP 错误映射
├── masq/         # 客户端伪装：指纹生成/持久化、会话管理、npm 版本同步、上游请求客户端
├── server/       # HTTP 服务层：路由分发、CORS 中间件、Panic 恢复、端点处理
├── translate/    # 协议转换核心：请求单跳映射、流式状态机（NDJSON → SSE）、非流式聚合
└── types/        # 协议数据结构定义（Anthropic、cmdc 信封与事件）
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
