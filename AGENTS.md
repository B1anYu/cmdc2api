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

1. `CC_ASSISTANT_REASONING=1` 时 `{type:reasoning}` assistant 历史块上游是否接受
   （形状是推测的，默认关闭即丢弃）。

## usage 语义定案（2026-09-09，三轮实测迭代后收口）

- **现象**：`cachedInputTokens / inputTokens` 恒定 ≈ 50%（49.2~50.0%，跨
  对话、跨版本稳定）；同一请求上游后台「总输入（缓存+未缓存）」≈ API
  `inputTokens` 的一半（80K vs 40K）。
- **最自洽模型**：cmdc 内部跑多步循环（tool 回合 step1 全量处理、step2
  全量命中缓存），`totalUsage` 按步求和 → I ≈ 2P、C ≈ P、C/I ≡ 50%；
  后台按去重后的真实 prompt 计费。该模型同时解释恒定比例、后台减半、
  以及 −2× 换算实测产出 input_tokens=0/命中率 100%（已证伪废弃）。
- **最终口径**：`input_tokens = inputTokens − cachedInputTokens`（钳 ≥0）
  ——在「多步求和」与「总量含缓存」两种模型下都等于真实口径，且与后台
  总输入对齐。
- **教训（两次方向性翻转）**：−1× → −2× 的依据是「与正常渠道形状对照」，
  但正常渠道 98% 命中 vs cmdc 恒定 50% 本身可能就是口径差异而非行为差异；
  −2× 上线后立刻被 0/100% 证伪。**换算公式必须能被线上数据证伪时快速
  回退——这次靠的正是这一点。**
- 复盘注：缓存排查初期「命中率 ~50%」的恐慌也源于此口径问题，缓存本身
  行为正常（C ≈ P 恰是 step2 全量命中的表现）；断点落点修复保留，属于
  合成路径的正确加固。

## 缓存与 usage 排查复盘（2026-09-08/09，结案）

- **最终定案以「usage 语义定案」节为准**：缓存一直正常（真实命中率 96~99.9%），
  「命中率 ~50%、cache_read 封顶 ~38.5K」是 cmdc `inputTokens` 重复计入缓存
  读取导致的网关显示假象，已由 usage 换算修复。
- 复盘教训：排查初期把显示假象误判为「断点钉死在静态前缀」，沿这条线做了
  两次断点位置修复 + 一轮静态分析，均为无效方向（对生产流量是空操作）。
  教训：**命中率类指标先核对分子分母的口径，再动协议层**。
- 排查线的实际留存收益（保留）：① 合成断点改为从信封整体末尾回扫——修掉
  合成路径的一个真实潜在缺陷（agent 链中断点永不前进），虽然生产中合成
  路径从未触发；② 标记落点可观测日志（info/WARN 二选一）；③ 断点落点
  回归测试。
- `CC_CACHE_MARKERS=replace`：备用诊断旋钮（剥客户端标记 + 强制末尾合成）。
  其预设使用前提（「信封含标记但缓存仍封顶」）已随定案消失，仅当下游
  显示再次异常时作为 A/B 手段。
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
