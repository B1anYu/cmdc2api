# cmdc2api 项目规则

## 项目定位

Anthropic Messages → cmdc 直转代理（Go，零外部依赖，静态二进制）。
协议事实的权威参照是 `reference/commandcode-proxy/proxy.mjs`（MIT，行号基于
commit `fcdb56a`）；转换架构手法参考 `reference/sub2api/backend/internal/pkg/apicompat`
（sparse clone）。两个克隆已 .gitignore，仅本地参考，不随仓库分发。

## 安全红线

- 「伪装官方客户端」反代，有上游风控封号风险：**实弹测试只用小号 + 低频，绝不用主号**，先验证账号存活。

## 关键设计决策（勿轻易回退）

- 转换路径：Anthropic → cmdc 信封**单跳直转**，绝不引入 OpenAI chat 中间层。
- 会话亲和优先级：显式 session 头 > 前缀派生（`CC_SESSION_STRATEGY=prefix`，默认，
  `sha256(system+tools+首条用户消息)`，同对话跨轮稳定）> per-key 12h+1h 轮换（`=key`，原版行为）。
- thinking 签名：确定性伪造（sha256+0x12 前缀，满足客户端浅校验）；上游不回传签名，
  入站历史里的签名不发上游。
- 丢弃字段（上游无对应能力，与原版一致）：`stop_sequences`、`top_k`、`metadata.user_id`、
  tool_result 的 `is_error`。
- 伪装自洽性：指纹/lifecycle/信封 environment/workingDir 全套 win32；upstream transport
  强制 HTTP/1.1 + 空 User-Agent（`h.Set("User-Agent","")` 在 Go 中确实抑制该头，已实测）。
- 状态（指纹/生命周期节流）落盘 `CC_STATE_FILE`，键为 sha256(key) 前缀，明文 key 绝不入盘。

## 待实弹验证（小号低频，不阻塞开发）

1. usage `inputTokens` 是否含 `cachedInputTokens`（当前按原版直接透传）。
2. `CC_ASSISTANT_REASONING=1` 时 `{type:reasoning}` assistant 历史块上游是否接受
   （形状是推测的，默认关闭即丢弃）。
3. cache_control 标记 + 前缀派生会话是否带来 `cache_read_input_tokens > 0`（验收标准 2）。

## 已知陷阱

- cmdc 强制 `params.system` 为字符串，数组直接 400 Validation error（真机验证过）。
- `go vet ./...` 会扫进 `reference/sub2api`（其 sparse clone 不含自身 go.mod）：
  克隆根已放一个空 go.mod 隔离该子树，勿删。
- cmdc 信封是 messages 风味而非 OpenAI chat：tool_choice 用 Anthropic 风味
  `{type:auto|any|tool|none}`（含 `none`），tools 用 `input_schema`，content 恒为块数组。
- CC CLI 版本号从 npm registry 每 24h 拉取（实测当前 1.50.1），fallback `1.50.1`。

## 子智能体使用

- 简单/单点查询不派子智能体，主 agent 直接做；子智能体仅用于复杂或大规模并行探索。
- 用户偏好：非复杂子任务希望用轻量（flash）模型。当前 Agent 工具无 model 参数无法指定，
  若客户端未来支持按此执行。
