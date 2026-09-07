# cmdc2api

Anthropic Messages → cmdc（Command Code）直转代理，Go 实现，静态二进制部署。

上游参考 [commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy)（MIT，Node.js 版），
协议转换架构参考 [sub2api](https://github.com/Wei-Shaw/sub2api) 的 `apicompat` 包。

## 与 Node 版（commandcode-proxy）的差异

| 方面 | Node 版 | 本项目 |
|---|---|---|
| 转换路径 | Anthropic → OpenAI chat → cmdc（两阶段，丢字段） | **Anthropic → cmdc 直转** |
| 用户消息图片 | 静默丢弃 | 透传（base64 → data URI；URL 原样） |
| tool_result 内图片 | 只拼接文本 | 搬迁到后续 user 消息（sub2api 手法） |
| cache_control | Anthropic 路径全丢 | part 级标记保留/合成 + 会话亲和 |
| 前轮 thinking 块 | 丢弃 | 默认丢弃，`CC_ASSISTANT_REASONING=1` 实验回传 |
| usage 记账 | tool 调用按 +20 估算 | 上游 totalUsage 直读，漏发时才估算 |
| 指纹持久化 | 内存（重启即换指纹） | 落盘 `/data/state.json`，重启不变 |
| 环境自洽 | 指纹 win32 + workingDir Linux 路径 | 全套 win32 伪装自洽（含 workingDir） |

## 端点

| 路径 | 说明 |
|---|---|
| `POST /v1/messages` | Anthropic Messages（流式 SSE + 非流式 JSON） |
| `GET /v1/models` | 模型列表（带 key 透传上游，否则硬编码回退） |
| `GET /health` | 健康检查 |

鉴权：`Authorization: Bearer user_xxx` 或 `x-api-key: user_xxx`，key 即 cmdc 账号 key，原样透传上游。

## 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` / `HOST` | `3050` / `127.0.0.1` | 监听地址（容器内需 `HOST=0.0.0.0`） |
| `CC_API_BASE` | `https://api.commandcode.ai` | 上游地址 |
| `CC_STATE_FILE` | `data/state.json` | 指纹/生命周期节流状态落盘路径 |
| `CC_MAX_BODY_MB` | `100` | 入站请求体上限 |
| `CC_SESSION_STRATEGY` | `prefix` | 会话亲和：`prefix`（同对话稳定会话）/`key`（原版 12h+1h 轮换） |
| `CC_ASSISTANT_REASONING` | `0` | 实验开关：assistant thinking 块以 `{type:reasoning}` 回传上游 |
| `CC_FAKE_NODE_VERSION` | `v22.21.0` | 信封 environment 字段的伪装 Node 版本 |
| `CMD_ZDR` | `0` | 附加 `x-cmd-zdr: 1` 仅走 ZDR 路由（也可逐请求头指定） |
| `CC_MODEL_REFRESH` | `5m` | 模型列表缓存时长 |

## 缓存亲和设计

cmdc 的 prompt cache 按会话粒度工作（上游 PR#10 把 `prompt_cache_key` 直接当 session-id 用）。本代理三层配合：

1. **显式 session 头优先**：入站 `x-session-id` / `x-claude-code-session-id` / `session_id`（≥8 字符）直接作为上游会话 ID；
2. **前缀派生**（默认）：无显式头时从 `sha256(system + tools + 首条用户消息)` 派生稳定 UUID 形会话 ID —— 同一对话跨轮次复用同一会话；前缀变化（工具集变更、上下文压缩）时自动换会话；
3. **part 级缓存标记**：入站 content 块上的 `cache_control` 原位保留（归一为已验证的 `{type:"ephemeral"}`）；system/tools 上的标记（cmdc 强制 system 为字符串，无法落在 part 上）折算为首个 user 消息末尾 text part 的合成标记（PR#10 验证过的位置）。

## 已知取舍

- **system 必须是字符串**：cmdc 上游硬约束（数组直接 400），块结构无法保留，缓存断点走上面的折算路径。
- **thinking 签名是确定性伪造**：第三方代理不可能铸造合法签名；采用 sha256+0x12 前缀方案满足客户端浅校验，同一文本签名稳定。上游不回传签名，入站历史里的签名不会发往上游。
- **丢弃字段**（上游无对应能力，与原版一致）：`stop_sequences`、`top_k`、`metadata.user_id`；`is_error` 不映射。
- **usage 语义待实弹验证**：`inputTokens` 是否含 `cachedInputTokens` 目前按原版直接透传。

## 部署

```bash
docker compose up -d   # ghcr.io/b1anyu/cmdc2api:latest，双架构 amd64/arm64
```

接 axonhub：渠道类型 `anthropic`，base_url `http://cmdc2api:3050`（挂 `axonhub_default` 网络），渠道 key 填 cmdc 的 `user_` key。

本地裸跑：

```bash
go build -o cmdc2api . && ./cmdc2api
```

## 风险提示

本项目属「伪装官方客户端」反代，有上游风控封号风险（同类项目已有封号先例）。
**测试/使用请用小号 + 低频，先验证账号存活，绝不用主号。**

## 开发

```bash
go test -race ./...   # 单测 + 端到端（假上游）
go vet ./...
```

目录结构：

```
internal/config    配置加载（纯 env）
internal/masq      客户端伪装层：指纹/会话/CLI 版本/lifecycle/上游客户端
internal/types     线上协议类型（Anthropic 入站、cmdc 信封/事件、SSE 事件）
internal/translate 协议转换：请求直转、流式状态机、非流式聚合
internal/server    HTTP 入口：路由/中间件/错误映射
```
