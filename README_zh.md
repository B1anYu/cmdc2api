# cmdc2api

[English](README.md) | 简体中文

Anthropic Messages API → cmdc（Command Code）直转代理。

Go 实现，零第三方依赖，静态二进制部署。命令行编程助手类客户端以 Anthropic 协议接入，
本代理将请求单跳转换为 cmdc 信封格式转发上游，完整保留图片、思考链与缓存标记，
并在请求层还原官方 CLI 的客户端指纹。

## 特性

- **单跳直转** —— Anthropic → cmdc 一步到位，不经 OpenAI chat 中间层
- **完整图片支持** —— 用户消息图片（base64 / URL）透传；tool_result 内嵌图片自动搬迁
- **缓存亲和** —— `cache_control` 标记保留与合成，配合稳定会话派生，最大化 prompt cache 命中
- **客户端伪装** —— 设备指纹、lifecycle 预请求、实时 CLI 版本号、OTel traceparent、传输层指纹对齐
- **流式 / 非流式** —— 上游恒流式，代理按入站需求输出 Anthropic SSE 或聚合 JSON，两种形态语义一致
- **准确计费语义** —— usage 直读上游终值；零输出响应按 429 处理并带 Retry-After，防止下游误计费

## 快速开始

Docker（推荐，双架构 linux/amd64 + arm64）：

```bash
docker compose up -d   # ghcr.io/b1anyu/cmdc2api:latest
```

本地裸跑：

```bash
go build -o cmdc2api . && ./cmdc2api
```

服务默认监听 `127.0.0.1:8050`。鉴权使用 cmdc 账号 key，原样透传上游：

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

## 端点

| 路径 | 说明 |
|---|---|
| `POST /v1/messages` | Anthropic Messages（流式 SSE + 非流式 JSON） |
| `GET /v1/models` | 模型列表（带 key 透传上游，否则回退内置列表） |
| `GET /health` | 健康检查 |

鉴权头：`Authorization: Bearer user_xxx` 或 `x-api-key: user_xxx`。

## 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` / `HOST` | `8050` / `127.0.0.1` | 监听地址（容器内需 `HOST=0.0.0.0`） |
| `CC_API_BASE` | `https://api.commandcode.ai` | 上游地址 |
| `CC_STATE_FILE` | `data/state.json` | 设备指纹与生命周期节流状态的持久化路径 |
| `CC_MAX_BODY_MB` | `100` | 入站请求体上限 |
| `CC_SESSION_STRATEGY` | `prefix` | 会话策略：`prefix`（按对话稳定）/ `key`（按 key 12h+1h 轮换） |
| `CC_ASSISTANT_REASONING` | `0` | 实验功能：将历史 thinking 块以 `{type:reasoning}` 回传上游 |
| `CC_FAKE_NODE_VERSION` | `v22.21.0` | 信封 environment 字段中的 Node 版本 |
| `CMD_ZDR` | `0` | 附加 `x-cmd-zdr: 1` 仅走 ZDR 路由（也可逐请求头指定） |
| `CC_MODEL_REFRESH` | `5m` | 模型列表缓存时长 |

## 设计说明

### 转换路径

cmdc 信封本身就是 messages 风味（content 为块数组、tools 用 `input_schema`、tool_choice为 Anthropic 风格），与 Anthropic 协议天然最近。本代理因此不做 Anthropic → OpenAI chat 的中间转换，请求侧字段一步映射，规避两阶段转换固有的字段丢失。

### 缓存亲和

cmdc 的 prompt cache 按会话粒度工作。本代理三层配合：

1. **显式 session 头优先**：入站 `x-session-id` / `x-claude-code-session-id` / `session_id`（≥8 字符）直接作为上游会话 ID；
2. **前缀派生**（默认）：无显式头时从 `sha256(system + tools + 首条用户消息)` 派生稳定的 UUID 形会话 ID —— 同一对话跨轮次复用同一会话；前缀变化（工具集变更、上下文压缩）时自动换会话；
3. **part 级缓存标记**：content 块上的 `cache_control` 原位保留（归一为 `{type:"ephemeral"}`）；system / tools 上的标记折算为首个 user 消息末尾 text part 的合成标记。

### 客户端伪装

对齐官方 CLI 的可观察行为：Windows x64 设备指纹（持久化，重启不变）、`/alpha/fingerprint/record` 与 `/alpha/lifecycle-events` 预请求（8h + 2h 抖动节流）、npm 实时拉取 CLI 版本号写入 `x-command-code-version`、按会话派生的 project-slug、W3C traceparent。传输层强制 HTTP/1.1 并不发送 User-Agent，与 Node CLI 的出站特征一致。

### 已知限制

- cmdc 强制 `params.system` 为字符串（数组直接 400），system 的块结构与缓存断点走上述折算路径；
- thinking 签名为确定性伪造（sha256 + 0x12 前缀）：第三方代理无法铸造真实签名，该方案满足客户端浅校验且同一文本签名稳定；
- 上游无对应能力的字段被丢弃：`stop_sequences`、`top_k`、`metadata.user_id`、tool_result 的 `is_error`；
- 上游 usage 语义（`inputTokens` 是否含缓存桶）按上游参考实现的口径直接透传。

## References

协议知识与客户端伪装来自 [commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy)（MIT），
转换架构手法参考 [sub2api](https://github.com/Wei-Shaw/sub2api)（LGPL-3.0）的 `apicompat` 包。
cmdc2api 本身为原创代码，未包含上述任一项目的源码。

## License

[MIT](LICENSE)

## 开发

```bash
go test -race ./...   # 单测 + 端到端（内置假上游）
go vet ./...
```

```
internal/config     配置加载（纯环境变量）
internal/masq       客户端伪装层：指纹/会话/版本号/lifecycle/上游客户端
internal/types      线上协议类型（Anthropic 入站、cmdc 信封与事件、SSE 事件）
internal/translate  协议转换：请求直转、流式状态机、非流式聚合
internal/server     HTTP 入口：路由/中间件/错误映射
```

## 免责声明

本项目仅供学习与技术研究使用。使用者应自行确保其使用行为符合所在地法律法规以及相关服务的条款；因使用本项目所产生的一切后果由使用者自行承担，项目作者不承担任何责任。
