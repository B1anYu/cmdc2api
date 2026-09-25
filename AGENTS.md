# cmdc2api 项目规则与开发指引

## 一、 项目定位与参照

- **项目目标**：多协议入站反向代理——Anthropic Messages（一等公民）、OpenAI Responses（面向 Codex CLI）与 OpenAI Chat Completions 三端点 → cmdc 单跳直转（Go 编写，零第三方依赖，静态二进制部署）。
- **当前版本**：`v0.2.0`（2026-09-17，tag 指向 `8d05b3e` 的多协议入站实现）。分支 `fix/verified-findings-2026-09` 上的缺陷修正在此之后，尚未发版。
- **权威参照**：
  - 协议事实与伪装标准：`reference/commandcode-proxy/proxy.mjs`（MIT，基于 commit `fcdb56a`）。
  - 协议转换技法参考：`reference/sub2api/backend/internal/pkg/apicompat`（LGPL-3.0，仅借鉴架构手法，未复制代码）。
- **`reference/` 目录隔离规则**：
  - `reference/` 已加入 `.gitignore`，不入库。
  - 新环境克隆方式：
    ```bash
    git clone https://github.com/MAXeaglet/commandcode-proxy.git reference/commandcode-proxy
    git clone --filter=blob:none --sparse https://github.com/Wei-Shaw/sub2api.git reference/sub2api
    git -C reference/sub2api sparse-checkout set backend/internal/pkg/apicompat
    printf 'module github.com/Wei-Shaw/sub2api\n\ngo 1.24\n' > reference/sub2api/go.mod  # 隔离子树，勿删
    ```

---

## 二、 安全红线

> [!CAUTION]
> 本代理属于「高拟真客户端伪装」反向代理，存在上游风控封号风险：
> - **实弹测试严禁用主号**：必须使用测试小号并在低频条件下验证。
> - **测试前先验证存活**：确保测试账号本身未被上游封禁。

---

## 三、 关键设计决策（核心不变式，勿轻易回退）

1. **鉴权机制与兼容性**：
   - 客户端鉴权头兼容 `Authorization: Bearer <key>` 与 `x-api-key: <key>`，取到的 key **原样透传上游**。
   - **代理本身不校验 key 的合法性**：`getAPIKey` 只做提取（`Bearer ` 之后的 token、`x-api-key` 原值，各自 TrimSpace），**不做前缀校验**（不再按 `user_` 正则过滤）。依据：上游未来可能改用别的 key 形态（如 `sk-` 开头），本地按前缀过滤会把「发了但格式不对」误报成「没发 key」；非法/未知形态的 key 一律送上游，由上游报错并原路返回客户端。
   - 只有确实**没发** key（两个头都取不到）时才本地 401，文案提示期望的 cmdc key 形状（`user_` 前缀）。该文案与状态码/错误类型在 `missingAPIKeyMessage` + `writeMissingAPIKey` 一处定义，三端点（messages/chat/responses）共用，防三份漂移。
   - 明文 API Key 严禁写入日志或持久化磁盘（`data/state.json` 中仅以 `sha256(key)` 前缀为索引）。
2. **单跳直转与星形拓扑**：客户端协议 → 规范格式（`types.Request`，Anthropic Messages 形状）→ cmdc 信封一步完成，**坚决不引入任何中间层**（含 OpenAI Chat），避免字段与类型丢失。Anthropic Messages 是一等公民与内部规范格式：`/v1/responses`（面向 Codex CLI）与 `/v1/chat/completions` 的入站归一化（`translate/respin.go` / `chatin.go`）插在 `BuildCcRequest` **之前**，会话亲和、缓存断点合成、伪装全部免费继承；出站协议差异全部收敛在 `types.StreamEvent` **之后**（`translate/respout.go` / `chatout.go`，与 `Aggregator` 同层；server 侧经 `pipeline.go` 的 `EventEncoder`/`EventAggregator`/`ErrorWriter` 三接口注入），`StreamTranslator` 与 `masq` 包对入站协议零感知。
3. **Responses/Chat 出站硬约束**（2026-09-17 定案，违反会破坏严格客户端尤其是 Codex）：
   - Responses SSE 帧逐事件手工构造（反 omitempty）：`output_index/content_index/summary_index` 的 0 必须出现；`function_call.arguments` 可为空串但键必须在；`message.content` 恒数组；reasoning item **绝不带 id**（OpenAI 404 未签发 id）且剔除 null 的 `status/content` 字段（C# SDK 崩溃）。
   - Responses 流**没有** `data: [DONE]`；Chat 流**必须**以 `data: [DONE]` 收尾。
   - `sequence_number` 从 0 单调递增；终态事件（`response.completed/incomplete/failed`）必须携带完整 output 数组与 usage。
   - 流式与非流式消费**同一状态机**产出（严禁双实现，防语义分裂）；流内 error 事件在编码器内转 `response.failed`；`Finish()` 必须幂等（管线无条件调用）。
   - 工具调用全量参数按 ~10 rune 分片模拟增量（上游一次性给全 input）。
   - Codex 工具族（custom/grammar、tool_search、namespace、additional_tools）入站降级为普通 function 工具，`ResponsesToolMapping` 由 respin 产出、respout 按其回程还原 item 形态（`fc_/ctc_/tsc_` id 前缀）；`previous_response_id` 一律 400 拒绝（Codex `store:false` 全量回放，不依赖服务端状态）。
   - tool_use/tool_result 配对修复（`translate/pairing.go` 的 merge→pair→merge）是双入站共享的正确性核心，三条不变式：结果紧邻前条调用、调用被下条结果应答、角色严格交替。
4. **会话亲和优先级**（`masq/session.go` 的 `ResolveSession` + `translate.PrefixCacheKey`）：
   - 优先级 1：显式 Session Header（`x-session-id` / `x-claude-code-session-id` / `session_id` ≥ 8 字符）；
   - 优先级 2：客户端显式声明的 `prompt_cache_key`（OpenAI Chat/Responses 字段，长度 ≥ 8 才采用）——它是客户端对「这条对话属于哪个缓存前缀」的显式声明，比我们派生的哈希更权威；依据参照 `proxy.mjs:204-210`（把该字段作为 x-session-id 候选之一）。⚠️ **按 §八 分域判据此处是 A−**（会话亲和怎么取键是工程选择，不是伪装常量、也不是信封字段形状）：它支撑「采用」这一方向的合理性，但不构成「必须这么取」的 A 级依据；该优先级的最终依据是用户亲批（`fix-plan.md` §6.0/B 组）；
   - 优先级 3：前缀哈希派生（`CC_SESSION_STRATEGY=prefix`，默认）：`sha256(system + tools + 首条用户消息)`，同对话跨轮稳定；
   - 优先级 4：Per-Key 轮换（`CC_SESSION_STRATEGY=key`，原版 12h + 1h 抖动轮换）。
   - **无论来源，输出都会再哈希一次并格式化为 UUID 形状**：该会话键还会进入 `WorkingDirForSession` 与上游请求头，不能让客户端可控字符串直接外泄到那些位置。
5. **Thinking 思考回传与签名**：
   - 上游 CC 在思考模式下强制校验历史中的思考过程，缺失会导致 `502 (The reasoning_content in the thinking mode must be passed back to the API)`；
   - 历史 assistant 思考块默认以 `{type: "reasoning", text: ...}` 格式回传上游（可通过 `CC_ASSISTANT_REASONING=0` 显式禁用），且严格遵循 CC CLI 抓包顺序 `[reasoning, text, tool-call]`；
   - Thinking 思考签名采用确定性伪造（`0x12` + sha256 前缀）满足下游客户端浅校验；上游不接受也不回传签名，历史记录中的签名发往上游时需过滤。
   - **思考档位值域**：`low`/`medium`/`high`/`max`/`xhigh` **原样透传**（参照 `README.md:100` 值域为 `low/medium/high/max`、`:365` 明写 pass-through、`proxy.mjs:512-513` 直接透传——⚠️ **按 §八 分域判据这三处都是 A−**（「该字段该填什么值」属 A−）；此处的结论**不依赖它们**——原样透传字段不需要值域依据，A− 只用于反对收窄。`xhigh` 另有**用户对上游的实测认知**作依据，A− 文档未列该档）；仅 `minimal → low`（**未验证的下映射**，`minimal` 不在值域内）。Anthropic 的 `thinking.budget_tokens` 仍按既有阈值折算（`≥10000→high`、`≥5000→medium`、`>0→low`，借自 **C 级** litellm，未经 A 级验证），`adaptive` 形态缺省 `medium`。⚠️ **原「上游只认 low/medium/high」与 `xhigh/max→high` 收窄的口径已被逐字段对账证伪，勿回退。** 证伪的支点是**「收窄＝我们自己造了值，而它没有任何 A 级或实弹出处」**（按硬性要求 1 独立成立，不需要参照作证）；参照的「透传」只是方向提示（A−），不得作为升格依据。
6. **安全丢弃字段**（上游无对应能力，对齐原版行为）：
   - Anthropic 入站：`stop_sequences`、`top_k`、`metadata.user_id`、`tool_result.is_error`。
   - Responses/Chat 入站追加：`truncation`、`include`、`background`、`service_tier`、`safety_identifier`、`user`、`metadata`、`text`（format/verbosity）、`top_logprobs`、`stream_options`、`logprobs`、`response_format`/`text.format`（结构化输出，安全丢弃不转换）；Chat 追加 `stop`（不映射、不落 `out.StopSequences`）、`frequency_penalty`、`presence_penalty`、`logit_bias`、`store`、`prediction`、`extra_body`（无类型字段由 `types.ChatIgnoredFields(raw)` 探测存在性后留痕）；Chat 的 `n>1` 直接 400。
   - **`prompt_cache_key` 不在本清单内**：它被**消费**为会话亲和候选（见 §三.4 优先级 2），既不是丢弃项、也无信息损失，故**不报 ignored、不留痕**。它由 `types.ChatRequest`/`ResponsesRequest` 的类型化字段读入并搬运到 `types.Request.PromptCacheKey`。
   - `top_p`：**有意静默丢弃——不转发上游、不回显、不留痕**。`BuildCcRequest` 三条入站共用且只转发 `temperature`，该参数从未到达上游；而它此前被回显在 Responses 响应对象（含流式终态事件）里，等于向客户端谎报「参数已生效」——**谎报才是真缺陷**。本项是「所有丢弃必须留痕」规约的**唯一显式例外**（2026-09-21 用户拍板：该参数几乎无人使用，不值得为它引入 warn）。修正经共用的 `BuildCcRequest` 对 **Anthropic 一等公民路径同样生效**。
   - `reasoning.encrypted_content` 丢弃**不留痕**（Codex 每轮必发，留痕会淹没日志）。
   - Anthropic 原生入站的**降级 / 钳制**（文案自带「哪个工具·哪个字段 + 我们做了什么 + 后果」，且**同一请求内聚合为一条**，避免长对话刷屏）：
     - 工具 `input_schema` 非 object（顶层 `anyOf`/`oneOf`、`type` 缺失或非 `"object"`、非法 JSON）→ 整体替换为 `{"type":"object","properties":{}}`（保守行为，**不改为透传**：透传 `anyOf` 无任何一级证据，而 MCP 工具联合 schema 被拒会使整轮 400）；
     - `tool_choice` 未知 `type` → 降级为 `auto`；`{type:"tool"}` 缺 `name` → 原样发出无名 `{"type":"tool"}`（这两个静默分支**仅 Anthropic 原生入站可达**：Chat/Responses 在各自预归一层已留痕）；
     - `max_tokens > 200000` → 钳制到 200000（**上限保留**，不提高、不改为纯透传）。
   - Responses 与 Chat 入站的**降级 / 换算**（口径与 Anthropic 侧一致，同请求内聚合）：
     - 工具 `parameters` 非 object（无参数、顶层 `anyOf`/`oneOf`、`type` 缺失或非 object、非法 JSON）→ 同样整体替换为 `{"type":"object","properties":{}}` 并聚合留痕；
     - 工具参数 `arguments` 非法 JSON → 兜底 `{}` 并留痕（原始文本就此丢失）；合法 JSON 但非 object → **原样透传**并留痕（**不擅自包一层**）——**两条入站路径共用同一实现**（`chatToolArgsReport`）；
     - **未知 `role` 的 message / input item → 降级为一条 user 消息（内容保留）**，留痕记「downgraded to a user message」。依据参照 `proxy.mjs:467-468`（其注释理由是「避免 CC 校验拒绝」）——⚠️ **按 §八 分域判据「要不要降级/容忍」属 A−**，故本条的行为变更依据是**用户亲批**（`fix-plan.md` §6.0 的 P2-19），参照只作方向提示。**取代此前的「整条丢弃」**；
     - Responses 的 `tool_search_output` **仅在 `status == "completed"` 时才提升**为正式工具声明（缺 `status` 不再被当作完成态）；
     - **`parallel_tool_calls` 有上游载体，不是丢弃项**：并入 `tool_choice.disable_parallel_tool_use`，经 `convertToolChoice` 落到信封**顶层 `params.parallel_tool_calls`**（`CcToolChoice` 只有 type/name，线格式里**没有**这个标志）。映射与 Chat 侧**共用** `chatApplyParallelToolCalls`，且必须在 `tool_choice` 落地**之后**合并。留痕口径两条入站一致：**仅「显式 `false` 但本轮无工具、无法表达」按行为性丢失 WARN**；显式 `true` 与未声明**不报**（`true` 就是上游默认行为，没有任何限制被忽略；对 `true` 也报会让「每轮都发 true」的客户端被无信息量的日志刷屏）。；
     - **回显语义**：Responses 的 `parallel_tool_calls` 未声明时回显协议默认 `true`、显式值回显其原值（旧实现**恒回 `false`**、与「任由上游并行」的实际行为方向相反）；
     - Chat 的 `function.arguments` 为**非字符串**（对象/数组/数字/布尔）时**不再让整轮 400**，按原始 JSON 文本透传（`types.chatFunctionArguments`，依据参照 `proxy.mjs:450/542-544/1256` 的全程容忍——⚠️ **按 §八 分域判据「要不要容忍」属 A−**；本条另有项目内一致性论据：F8 的立论是「一条 tool_call 的形态问题不该把整轮请求打成 400」）；这属**形态换算、非丢弃，故不留痕**。
   - **绝不覆写**（非丢弃项，登记于此以防文档与代码互相矛盾）：客户端 part 级 `cache_control`（含 `ttl`）**原样透传**，仅当 `type` 缺失时补 `"ephemeral"`，**无 ttl 时不合成 ttl**——cmdc 之下还有它自己的上游（deepseek 等默认 24h 缓存），覆写 ttl 会真的拉低其默认缓存时长（见 `types.CacheControl` 注释）。
   - **出站 item 生命周期不变式**：任何 item 在**每条终结路径**（正常收尾、流内 error 事件、上游半截断开）上都必须**先 part done、再 item done**；`reasoning` 块同样如此（此前只有 message 文本块那半做了收尾，思考块漏了，严格客户端会看到 part 永久悬空）。`tool_search_call.arguments` 线上**恒为对象**（解析结果为 `null`/空/非预期类型时归一为 `{}`）。
   - 所有丢弃必须留痕（`info:` 前缀为良性观测，其余 warn），两条入站路径口径一致（空 tool_result 归一化为 `""`）。思考档位值域见 §三.5。
7. **客户端伪装自洽性**：
   - 指纹/Lifecycle/信封 Environment/WorkingDir 全套对齐 Windows x64。
   - 出站 Transport 强制 `HTTP/1.1` 且置空 `User-Agent`（Go 中 `h.Set("User-Agent", "")` 可抑制默认 UA）。
8. **Token Usage 换算**：优先采用上游 `inputTokenDetails.noCacheTokens` 原生非缓存输入；缺失时回退到 `max(0, inputTokens − cachedInputTokens)`，严禁改回无脑透传（详见下节）。

---

## 四、 Usage 计费口径（2026-09-09 定案与 2026-09-12 升级）

### 1. 现象与实测
- 上游 API 返回的 `cachedInputTokens / inputTokens` 比例恒定 $\approx 50\%$。
- 相同请求在上游控制台记录的「总输入」正好为 API `inputTokens` 的一半（例如 80K vs 40K）。

### 2. 模型机理与新字段
- cmdc 内部多步循环按步累加 usage（Step 1 全量处理产生 P、Step 2 全量命中缓存产生 P 缓存读取）→ I ≈ 2P, C ≈ P。
- 上游近期已在 `inputTokenDetails.noCacheTokens` 原生回报去重后的非缓存输入 tokens（满足 `noCacheTokens + cacheReadTokens === inputTokens`）。
- 上游后台按去重 Prompt 计费。

### 3. 定案公式
```text
input_tokens = (inputTokenDetails.noCacheTokens != nil) ? noCacheTokens : max(0, inputTokens - cachedInputTokens)
```
- 优先读取上游原生 `noCacheTokens`；若缺失则执行减法兜底。
- 在「多步求和」与「总量含缓存」两种模型下均等于真实消耗，且与上游后台总输入一致。
- 抽验指标：网关 `input_tokens` 必须与 cmdc 后台总输入统计对齐。

### 4. 出站协议的 usage 回填（2026-09-17 定案）

内部 canonical 保持上述 noCacheTokens 口径**不变**；Responses/Chat 出站按其协议语义（input/prompt tokens 为**含缓存总量**，与 Anthropic 协议相反）做加法回填，换算统一走 `types.ResponsesUsageFromDelta`（单一实现，流式/非流式共用）：

```text
input_tokens / prompt_tokens = noCache + cache_read (+ cache_creation 若 >0)
input_tokens_details.cached_tokens / prompt_tokens_details.cached_tokens = cache_read
total_tokens = input_tokens + output_tokens
```

---

## 五、 Prompt Cache 缓存断点机制

1. **缓存正常表现**：$C \approx P$（50% 比例）正是第二步全量命中缓存的正常表现，非缺陷。⚠️ **早期曾据此误判「命中率异常」**——真实原因是统计口径把 token 算了两遍、使命中率永远 ≤50%；该误判当初催生了「断点放信封末尾」的设计。**测量命中率前必须先确认不受统计口径干扰**，否则会再次被假象带偏。
2. **断点合成位置**：从**信封整体末尾**向前回扫最后一个 `text` part（勿改回只扫 `user` 消息 —— Agent 交互链尾部常为 `role: "tool"`，只扫 user 会停滞在首条人类消息）。
3. **客户端标记优先**：
   - `CC_CACHE_MARKERS=respect`（默认）：优先透传客户端 part 级标记，仅在缺失时进行末尾合成。
   - `CC_CACHE_MARKERS=replace`：剥离客户端标记并强制末尾合成（仅用于 A/B 诊断）。
4. **两条新端点如何拿到断点**：OpenAI Chat 与 Responses 协议**没有 `cache_control` 字段**，因此入站归一化层在 `types.Request` 上置 `SynthesizeTailCacheMarker`，信封构造据此在末尾合成断点——否则这两个端点永远没有 part 级断点，只剩会话亲和一条缓存路径。**Anthropic 一等公民路径不置该标志**（它本就能表达 part 级标记），故其现有行为不变。
5. **断点可观测性**：
   - 透传日志：`info: ... marker(s) present in envelope`
   - 合成日志：`WARN: no part-level cache_control present in the envelope; synthesized ...`（文案成因中立，覆盖「system/tools 标记折算」与「协议本无该字段」两种情形）
6. **⚠️ 未决的设计问题（勿当既定结论用）**：
   - 断点**位置**的正当性存疑（本项目用「信封末尾」；参照 `proxy.mjs:472-476` 用「首条 user 消息的最后一个 text part」——⚠️ **按 §八 分域判据「该往哪放」属 A−**，故不得据此单方面改位置），且第 1 条所述的驱动理由已被证伪。留待**缓存专项轮次**研究。
   - 按本节的字面口径，`respect` 模式下「无任何标记时也应合成」——但 Anthropic 入站目前只在 `sysCC||toolsCC` 时合成。该口径差**尚未裁决**，勿擅自改动 Anthropic 路径行为。
   - 多断点（上限 4 的钳制，仅 C 级旁证）在新增多断点前必须补钳制。
   - **工具级 `cache_control` 不进入信封**，其 `ttl` 因此不生效（见 §三.6）。

---

## 六、 环境与发版流水线

### 环境变量全量清单（以 `internal/config/config.go` 为准）

| 变量 | 默认值 | 作用 |
|---|---|---|
| `PORT` | `8050` | 监听端口 |
| `HOST` | `127.0.0.1` | 监听地址（容器内需 `0.0.0.0`） |
| `CC_API_BASE` | `https://api.commandcode.ai` | 上游基址 |
| `CC_STATE_FILE` | `data/state.json` | 设备指纹与 lifecycle 节流状态 |
| `CC_MAX_BODY_MB` | `100` | 请求体上限 |
| `CC_MODEL_REFRESH` | `5m` | `/v1/models` 缓存 TTL（**按 apiKey 分别缓存**） |
| `CC_SESSION_STRATEGY` | `prefix` | 会话亲和策略：`prefix` / `key` |
| `CC_CACHE_MARKERS` | `respect` | 断点策略：`respect` / `replace` |
| `CC_ASSISTANT_REASONING` | `1` | 是否回传历史思考块（`0` 禁用） |
| `CC_FAKE_NODE_VERSION` | `v22.21.0` | 信封 `environment` 里上报的 Node 版本 |
| `CMD_ZDR` | `false` | 置 1 时全局附加 `x-cmd-zdr: 1` 头（可被单请求覆盖） |

（既有说明：`CC_SESSION_STRATEGY` 见 §三.4；`CC_CACHE_MARKERS` 见 §五.3；`CC_ASSISTANT_REASONING` 见 §三.5。）

### 本地构建与测试

```bash
go vet ./... && go test -race -count=1 ./...     # 提交前必跑；-race 是硬要求
go test -race -count=3 ./...                     # 收工时确认无抖动（超时类测试最易抖）
go build -o cmdc2api . && ./cmdc2api             # 本地起服务，默认 127.0.0.1:8050
docker compose up -d                             # 容器方式（挂 ./data:/data，容器内须 HOST=0.0.0.0）
```

- 单条提交的隔离验证：`git worktree add --detach /tmp/v <sha> && (cd /tmp/v && go test ./...)`，用完 `git worktree remove`。
  （只跑工作树的测试无法验证「单个提交是否自洽」，这个做法可以。）

### 开发与部署

- **开发与部署环境**：开发与部署均在 VPS。
- **构建流**：`git push origin main` $\rightarrow$ GitHub Actions CI（vet / test -race / lint）+ GHCR `:dev` 双架构镜像。
- **发版流**：
  - 发版步骤：`git tag vX.Y.Z && git push origin vX.Y.Z`（**Tag 必须单独 push**，避免被 paths-ignore 吞掉）$\rightarrow$ 触发 GitHub Release 并发布 `:latest` 与 `:X.Y.Z` 镜像。
  - **`:latest` 仅随 Tag 更新，不跟 `main`**。VPS 若部署 `:latest`，合入关键修复后务必打 Tag。
> [!IMPORTANT]
> **镜像更新与交叉编译发版强绑定 Tag**：
> - `:latest` Docker 镜像与多平台交叉编译二进制（GitHub Releases）**仅在推送 Tag 时触发构建**；普通 push 到 `main` 仅触发 `:dev` 镜像构建。
> - 若需让 `:latest` 镜像更新或发布多平台二进制，务必执行发版推送：`git tag vX.Y.Z && git push origin vX.Y.Z`。
- **状态迁移**：换机迁移时务必带上 `data/state.json`，确保设备指纹连续稳定。

---

## 七、 已知陷阱与避坑指南

1. **System 参数类型**：cmdc 强制要求 `params.system` 为字符串，传入数组直接触发 `400 Validation error`。
2. **Reference 子树隔离**：`reference/sub2api` 根目录的空 `go.mod` 用于阻止 `go vet ./...` 递归扫描，不可删除。
3. **cmdc 信封格式**：
   - `tool_choice` 使用 Anthropic 风格 `{type: "auto"|"any"|"tool"|"none"}`（包含 `none`）。
   - `tools` 字段使用 `input_schema`。
   - `content` 恒为块数组。
4. **CLI 版本动态获取**：CC CLI 版本号每 24h 从 npm registry 动态同步（fallback `1.50.1`）。
5. **Go 默认 HTTP 行为**：Go `net/http` 默认协商 HTTP/2 且携带 `Go-http-client` UA，必须在 Transport 强制 `HTTP/1.1` 并显式抑制 UA。
6. **Responses/Chat 协议事实**（2026-09-17 新增）：
   - `output_item.added` 必须带 item 名字（function_call 靠 name 路由），名字晚到时须延迟宣告并重放缓冲参数。
   - 交错思考（text→thinking）时必须先关旧 item 再开新 item，否则 `response.completed` 只带 reasoning（「成功但无输出」）；`content_index` 只在 part 关闭时推进，否则同 item 第二个 part 覆盖第一个。
   - 流式 `message_start` 级事件在管线中先缓冲（延迟 200 语义），首帧前错误回退 JSON。
   - cmdc 上游 content part 的线格式是 `tool-call`/`tool-result` + `toolCallId`/`toolName`（挂在 role `"tool"` 消息上），**不是** Anthropic 的 `tool_use`/`tool_result` 命名。
   - 空 thinking 块出站只丢「空且无签名」的（带签名的必须发，客户端回放需要）；出站伪造签名沿用 `FakeThinkingSignature`，作为 Responses 的 `encrypted_content` 下发可自洽往返（入站还原为 thinking 块、发上游前过滤）。
   - **`disable_parallel_tool_use` 的线上落点是信封顶层 `params.parallel_tool_calls`**，不在 `tool_choice` 对象里——`convertToolChoice` 把它读成 `CcToolChoice` 的**兄弟字段**写入 `CcParams`。断言该行为时看 `params.parallel_tool_calls`。

---

## 八、 假设台账与交付前对账（防「四重闭环」复发）

**背景**：本项目发生过一次事故——`reasoning_effort` 的 `{minimal→low, xhigh→high, max→high}` 收窄**无任何出处**（真实来源是 litellm 的 `together_ai` provider 专用适配器，属 C 级），却经「规格无出处断言 → 实现忠实执行 → 测试锁定假设 → 本文件固化」四重闭环一路合入，直到以 A 级参照逐字段对账才发现。同类事故还在缓存断点处重演过一次（评估阶段想当然「新端点会自动继承断点合成」）。

### 参考源效力分级（写作与审查都按此执行）

| 级别 | 内容 | 可用性 |
|---|---|---|
| **A 级** | ① 抓包、上游报错回执；② **本机真 CLI**（npm 包 `command-code`）的伪装常量、头集、以及它**实际发出**的线格式（存在性证明：它发了 ⇒ 上游接受）；③ `reference/commandcode-proxy/` **仅限两域**：(a) **客户端伪装**（指纹、头集、UA、CLI 版本、lifecycle、workingDir、environment）；(b) **cc 信封线格式**（`params.*` 形状、content part 命名、tools/tool_choice/system 形态、断点字段形状） | **可作值域与字段行为依据**（②③ 的「没发／没覆盖」是**零信息**，不是禁止） |
| **A−** | 同一参照的**其余全部内容**——协议转换、映射表、容忍/降级策略、阈值、超时、错误码、usage 换算口径 | 只作方向提示与「有这种做法」的旁证；**不得单独作为值域/字段行为依据，不得单独决定改行为**（改行为需上游回执或本机实弹） |
| **B+** | 针对同一上游的**其它**独立实现建模（如 axonhub 的 `PlatformCommandCode` 能力矩阵） | 强旁证 |
| **C 级** | 多上游网关的内部启发式（litellm / CLIProxyAPI / sub2api 的映射表、默认值、阈值） | **只能证明「有这种做法」，不得作为值域依据** |

**为什么把参照劈成两半（2026-09-24 定）**：`reference/commandcode-proxy/` **不是抓包**，它是**同一上游的姊妹实现**——只有伪装与信封这两域是「不拿真机/真 CLI 就写不出来」的，其余都是作者在既有材料上自造的兼容层（可能是充分实弹的结论，也可能只是 vibe 一把梭，我们无法区分）。参照因此**从「权威」降为「最接近上游的姊妹实现」**：两域内仍是 A，两域外降 A−。

**分域判据（一句话）**：问「这条事实**只有拿到真机 / 真 CLI 才可能知道**吗？」——是 ⇒ 伪装或信封，A 级；否（是作者拿着既有材料做的工程选择）⇒ A−。典型落点：**字段形状**是 A，「该字段该填什么值 / 该往哪放 / 要不要容忍」是 A−。

**推论（透传字段不需要值域依据）**：A− 卡的是「**我们要自己造值**」的场合（`budget_tokens → effort` 这类映射、降级、钳制、默认值）。若某字段是**原样透传**的，客户端发什么我们发什么，就不存在「我们选了什么值」这一步，值域问题不成立——此时 A− 只用于反对「**收窄/映射**」，因为收窄本身就是「我们造了值」，而它的出处（若有）必须是 A 级或实弹。这也解释了 `reasoning_effort` 事故的真实形态：**事故不是「没听参照的话」，是「在没有 A 级出处的情况下自己造了值」**——按硬性要求 1 独立成立，无需参照作证。

**为什么真 CLI 也要打折扣（2026-09-24 补）**：真 CLI 虽是最高一档，但它的权威是**单向的**：

- **它发了 ⇒ 上游接受**（厂商自己的客户端就这么发，否则它自己先坏）——可作 A 级。
- **它没发 ⇒ 零信息，不是禁止**。（它从不发 `cache_control.ttl`、从不发 `top_p`、从不发 >64000 的 `max_tokens`——这些都不构成「上游拒绝」。）
- **它对「语义/效果」无话可说**：`command-code` 后端本身就是**聚合层**（转售各家 API，自身几乎没有算力），CLI 的信封**不是任何 provider 的原生格式**。CLI 内部就有 `declarativeWire()` 按 provider 在 `anthropic` / `openai-responses` / `openai` / `openai-compatible` 四种线格式之间选择，这直接证明**链路里必然存在再转换**。⇒「cmdc 接受」≠「provider 生效」。字段的**语义与效果**（缓存是否真的命中、档位被怎么解释）**只有本机实弹或上游回执能定**。
- **版本敏感**：该包一天发多个版本（450 个版本，2025-08 起）。镜像在 `reference/commandcode-cli/`（只读、`/reference/` 已 gitignore，不入库）：每个版本一个 commit + 一个 tag，`git diff 1.58.1 1.65.0` 即可看漂移，`git show <tag>:extracted/cli.mjs` 取任意版本原文。任何「对齐某版本」的结论都要标注版本号。

### 硬性要求

1. **每条上游能力断言必须标注出处**；无出处就写 `未验证`，且**不得作为约束依据**。
2. **禁止用 C 级来源作值域依据**。
3. 审查基准是「**实现 vs 权威参照逐字段对账**」，不是「实现 vs 需求」——只有前者能发现「本地收窄 vs 参照透传」这类偏差。**对账范围限于参照的 A 级两域**；两域外属 A−，「与参照不一致」本身不是改行为的理由。
4. 「阶段自报」≠「当前事实」：子代理在某一阶段报告「未修/待后续」的项，可能已被后续阶段修掉；**入清单前必须对 HEAD 复核**（`git show HEAD:<file>`）。本项目已出现 3 例此类伪发现（`readBody`、非流式 `incomplete_details`、reasoning 往返不变式）。
5. **A 级沉默 ≠ 实现有错**：若 A 级对该能力无覆盖，保守实现可以保留，但**必须补留痕**并把注释里「已验证」之类的失实表述改掉；只有在 A 级**明确反对**时才改行为。（A− 的沉默与「A− 也这么做」同理，都不构成依据。）
6. **禁止互证升格**：「参照（尤其是其 A− 部分）与我们做法相同」**不构成**出处，也不构成升格依据——参照自身可能只是某个第三方材料的二传手。实例：`proxy.mjs` 自写「Anthropic thinking → `reasoning_effort`（**LiteLLM 标准映射**）」⇒ 它的 `budget_tokens` 阈值与 **C 级** litellm 同源，拿它与我们互证等于自证。这是「四重闭环」的第五重变体：**用同源二传手互相担保**。升格的唯一路径是**上游回执或本机实弹**。

### 第三类问题：上游知识渗进入站转换层（架构级）

本项目的转换按「协议 → 规范格式 → 上游信封」组织，而**最容易出事的是第三条界线**：**上游专属知识被实现进了「协议 → 规范格式」这一段**。它的真值来源是 cmdc 的行为（值域、形状、约束），却住在了协议转换层——与转换算法无关，纯粹是知识放错了层。

**判据（一句话）**：问「它是因为**入站协议**是这样，还是因为**上游**是这样？」——后者即第三类。上游知识该住信封层（`request.go` 的 `BuildCcRequest`），不该住 `chatin.go` / `respin.go`。

**根因**：规范格式选的是 Anthropic Messages（三个入站协议之一的语言），而 cmdc 所需的表达能力**不是它的子集**——多出来的部分无处安放，只能渗进离它最近的那一层。

**三个识别特征**（照着 grep 就能找到，附现成实例）：

| 特征 | 现成实例 |
|---|---|
| 在规范类型上打洞：`json:"-"` 标志位，注释自承「仅供入站归一化层设置」 | `types.Request` 的 `SynthesizeTailCacheMarker`、`PromptCacheKey` |
| 侧表作额外返回值：信息装不进规范类型，只能挂在函数签名旁 | `ResponsesToolMapping` |
| 把上游方言直接写进规范字段 | `chatin.go` / `respin.go` 往 `Request.Thinking` 写 `{type:"adaptive", effort:…}`，迫使信封层的 `thinkingEffort()` 同时辨认两种方言 |

**危害**（三条均在本项目实际发生）：

1. **知识放错层 = 知识不存在**：审查者只会去信封层找「上游认什么」，于是永远找不到正确答案——`reasoning_effort` 事故就是这么活下来的（值域表住在名为 `chatReasoningEffort` 的函数里，名字听上去只是「读个字段」）。
2. **同一规则被两条入站路径放在不同的层**：工具 schema 形状规范化，Chat 侧交给信封层（`chatin.go` 注释明写「交给 `normalizeSchema`」），Responses 侧在声明时自己做（`respinToolSet.add`）——只因**去重键**要用规范化后的文本。
3. **上游形状改写污染协议语义判断**：同上，被上游规则改写过的 schema 文本被当作去重键，于是「重名工具该不该报错」这个协议语义判断，被一条上游格式规则左右。

**拓扑规律（最值得迁移的一条）**：同一个「用 Anthropic 当规范格式」的决定，**请求方向（三源 → 一汇）有害、回站方向（一源 → 三汇）有益**——请求方向有多个源，另外两个源被迫用规范语言说上游才懂的话；回站方向只有一个源，源适配只需一份，汇的差异是真实协议差异。⇒ 别问「这个模式好不好」，问「**在我这个方向的拓扑下好不好**」。

**处置（2026-09-23 定）**：**不重写，只贴标签**。未来改动集中在 `masq/`（客户端特征）与信封层（本就是干净的那一半），第三类不会被频繁触碰；而重写要冒协议正确性的险——那类问题只在 Codex / C# SDK 这类严格客户端上暴露，单元测试提前抓不到。**触发器：新增第 4 个入站协议时**——那时第三类成本会复利（每条上游规则都要在新入口重实现一遍），才是重写的时机。若届时重画，目标不是新架构，而是「让 OpenAI 两条路径变成 Anthropic 路径现在的样子」：阶段 1 只回答「客户端说的是什么」，阶段 2 只回答「上游要什么」（唯一一处，旁边挂证据等级），两者之间用一个显式的小载体承接规范格式装不下的信息。

### 交付前对账清单（改动涉及入站字段时逐条走）

- [ ] 本次新增/修改的每个字段，其行为是否有 A 级（或用户实测）出处？写进注释了吗？（出处仅到 A− 时必须显式标注 A− 与「未验证」，且不得据此单独改行为。）
- [ ] 三条入站路径对同一语义的处理是否**口径一致**？有跨路径一致性测试吗？
- [ ] 本次改动里，有没有「**因为上游是这样**」的判断落在了入站转换层？（第三类问题，见上。判据：因为入站协议是这样，还是因为上游是这样。是则移入信封层，或至少注明「此处属上游适配、非协议语义」。）
- [ ] 每个丢弃 / 降级 / 钳制 / 覆写都留痕了吗？文案是否自带「哪个字段 + 做了什么 + 后果」？同请求内是否聚合？
- [ ] 涉及的出站 item 生命周期，在**每条终结路径**上都闭合吗（不只是正常收尾）？
- [ ] 本文件与代码是否一致？（`stop` 死映射、`prompt_cache_key` 曾被列为「已丢弃」、失效的版本号，都是文档/代码互相矛盾的实例。）
- [ ] 新增测试是否**真的会失败**？（临时破坏被测行为确认变红，再还原。）

---

## 九、 智能体协作指引

- **主动发版 Tag 提醒规范**：
  - **触发时机**：当协助完成功能新增、Bug 修复或准备交付部署时。
  - **行动要求**：智能体须主动提示用户提交 Tag，明确说明 `:latest` 镜像与多平台交叉编译二进制均依赖 Tag 触发构建，并附带具体命令：`git tag vX.Y.Z && git push origin vX.Y.Z`。

