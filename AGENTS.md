# cmdc2api 项目规则

## 项目定位

Anthropic Messages → cmdc 直转代理（Go，零外部依赖，静态二进制）。
仓库：github.com/B1anYu/cmdc2api（public，MIT，LICENSE 已附）。
协议事实的权威参照是 `reference/commandcode-proxy/proxy.mjs`（MIT，行号基于
commit `fcdb56a`）；转换架构手法参考 `reference/sub2api/backend/internal/pkg/apicompat`
（sparse clone，LGPL-3.0，仅借鉴技法、未复制代码）。

**reference/ 不入库**（.gitignore），换机器后需重新获取：

```bash
git clone https://github.com/MAXeaglet/commandcode-proxy.git reference/commandcode-proxy
git clone --filter=blob:none --sparse https://github.com/Wei-Shaw/sub2api.git reference/sub2api
git -C reference/sub2api sparse-checkout set backend/internal/pkg/apicompat
printf 'module github.com/Wei-Shaw/sub2api\n\ngo 1.24\n' > reference/sub2api/go.mod  # 隔离子树，勿删
```

## 环境与发布（2026-09-09 起开发迁移至 VPS）

- 开发、部署同在 VPS：push main → CI（vet / test -race / lint）+ GHCR `:dev` 双架构构建；
  部署更新 = `docker compose pull && docker compose up -d`。
- 发版：`git tag vX.Y.Z && git push origin vX.Y.Z`（**tag 单独 push**，与 paths-ignore
  共存时混在普通 push 里可能被吞）→ `:latest` + `:X.Y.Z` + GitHub Release。
- 纯文档/compose 改动已被两个 workflow 的 paths-ignore 自动跳过；临时跳过用 `[skip ci]`。
- 指纹状态在部署目录 `data/state.json`：换部署位置时一并迁移，保指纹连续
  （重启不变是刻意设计，频繁全换指纹是风控特征）。

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
   两读均与实测数据吻合，唯一硬判据是上游计费口径；若确认含缓存桶，
   需改为 `input_tokens = inputTokens - cachedInputTokens`（钳 ≥0）。
2. `CC_ASSISTANT_REASONING=1` 时 `{type:reasoning}` assistant 历史块上游是否接受
   （形状是推测的，默认关闭即丢弃）。

## 缓存实弹结论与排查（2026-09-08/09，已定案）

- 实测（~78-83K 输入的长对话）：缓存读取恒定封顶 ≈ 静态前缀（~38.5K），命中率被
  稀释到 ~50%，缓慢爬升（38,528→41,344）= 用户偶尔发新文本使断点前跳。
- **静态分析定案根因（2026-09-09）**：synthesizeCacheMarker 原来只扫 role=="user"
  的消息，而 agent 式会话（一条人类消息 + 连续工具回合）的信封尾部全是 role:"tool"
  消息——断点永远钉在第一条人类消息 ≈ 静态前缀。此前两次「移到最后一条 user
  消息」的修复在 agent 链里是空操作（最后一条 user 消息就是第一条）。
  **已修**：从信封整体末尾回扫最后一个 text part（含 assistant 文本，位置随工具
  回合前进）；纯 tool_result 轮有一轮迟滞（非 text part 标记接受度未实弹验证，不赌）。
- **A/B 旋钮已备**：`CC_CACHE_MARKERS=replace`（剥全部入站 part 级标记 + 强制末尾
  合成）。若修复部署后日志显示「信封含标记但缓存仍封顶」→ 是 cmdc 对客户端标记
  的消费语义问题，翻这个开关对比。标记落点可观测：每请求二选一日志——
  `info: ... marker(s) present in envelope`（透传在生效）/ `WARN no part-level
  cache_control found`（合成兜底在生效）。
- 未验证：tool_result part 上挂标记是否被接受（现只落在 text part 上）。

## 已知陷阱

- cmdc 强制 `params.system` 为字符串，数组直接 400 Validation error（真机验证过）。
- `go vet ./...` 会扫进 `reference/sub2api`（其 sparse clone 不含自身 go.mod）：
  克隆根已放一个空 go.mod 隔离该子树，勿删（新机器重取克隆后同样要放）。
- cmdc 信封是 messages 风味而非 OpenAI chat：tool_choice 用 Anthropic 风味
  `{type:auto|any|tool|none}`（含 `none`），tools 用 `input_schema`，content 恒为块数组。
- CC CLI 版本号从 npm registry 每 24h 拉取（实测当前 1.50.1），fallback `1.50.1`。
- Go 客户端默认行为会破坏伪装：net/http 默认协商 h2 且发 `Go-http-client` UA；
  upstream transport 已强制 HTTP/1.1 + 空 UA，勿动。

## 子智能体使用

- 简单/单点查询不派子智能体，主 agent 直接做；子智能体仅用于复杂或大规模并行探索。
- 用户偏好：非复杂子任务希望用轻量（flash）模型。当前 Agent 工具无 model 参数无法指定，
  若客户端未来支持按此执行。
