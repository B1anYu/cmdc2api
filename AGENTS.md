# cmdc2api 项目规则

## 项目定位

Anthropic Messages → cmdc 直转代理（Go，零外部依赖，静态二进制）。
仓库：github.com/B1anYu/cmdc2api（public，MIT，LICENSE 已附）。
当前版本 v0.1.2（2026-09-09）。
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
- **`:latest` 只随 tag 更新，不跟 main**——VPS 若部署 `:latest`，有意义修复合入后
  要及时打 tag，否则拉到的永远是旧版（v0.1.0~v0.1.1 间曾因忘打 tag 排查混乱）。
  追 bleeding edge 用 `:dev`。
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
- usage 换算：`input_tokens = inputTokens − cachedInputTokens`（钳 ≥0），见下节，勿改回透传。

## usage 口径（2026-09-09 定案，三轮实测迭代）

- **现象**：`cachedInputTokens / inputTokens` 恒定 ≈ 50%（跨对话跨版本稳定）；
  同请求上游后台「总输入」≈ API `inputTokens` 的一半（80K vs 40K）。
- **最自洽模型**：cmdc 内部多步循环按步求和 usage（step1 全量处理、step2 全量
  命中缓存）→ I ≈ 2P、C ≈ P；后台按去重 prompt 计费。恒定 50%、后台减半、
  以及 −2× 换算实测产出 input_tokens=0（已证伪废弃）均由此解释。
- **最终口径**：`input_tokens = inputTokens − cachedInputTokens`——在「多步求和」
  与「总量含缓存」两种模型下都等于真实口径，且与后台总输入对齐。抽验方法：
  网关 `input_tokens` 应与 cmdc 后台总输入相等。
- **教训**：换算公式必须保留被线上数据快速证伪回退的能力；命中率类指标先核对
  分子分母口径，再动协议层（−1×→−2×→回退翻转了一次半）。

## 缓存断点（结案）

- 缓存行为正常：C ≈ P（恒定 50% 比值）恰是 step2 全量命中缓存的表现，非缺陷。
- 合成断点从**信封整体末尾**回扫最后一个 text part（勿退回只扫 user 消息——
  agent 链尾部全是 role:"tool" 消息，只扫 user 会钉死在第一条人类消息）。
- 客户端 part 级标记透传优先（有则不合成）；`CC_CACHE_MARKERS=replace` 为备用
  诊断旋钮（剥客户端标记 + 强制末尾合成），仅当下游显示再次异常时作 A/B。
- 标记落点可观测：每请求二选一日志——`info: ... marker(s) present in envelope`
  （透传）/ `WARN no part-level cache_control found`（合成兜底）。
- 未验证：tool_result part 上挂标记是否被接受（现只落 text part）；`CC_ASSISTANT_REASONING=1`
  的 `{type:reasoning}` 历史块上游是否接受（默认关闭即丢弃）。

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
