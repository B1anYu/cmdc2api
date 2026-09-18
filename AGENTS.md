# cmdc2api 项目规则与开发指引

## 一、 项目定位与参照

- **项目目标**：多协议入站反向代理——Anthropic Messages（一等公民）、OpenAI Responses（面向 Codex CLI）与 OpenAI Chat Completions 三端点 → cmdc 单跳直转（Go 编写，零第三方依赖，静态二进制部署）。
- **当前版本**：`v0.1.4`（2026-09-17）。
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
   - 客户端鉴权头兼容 `Authorization: Bearer user_xxx` 与 `x-api-key: user_xxx`。
   - 鉴权 key（cmdc 格式通常为 `user_xxx`）原样透传给上游，代理本身不校验 key 的合法性。
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
4. **会话亲和优先级**：
   - 优先级 1：显式 Session Header（`x-session-id` / `x-claude-code-session-id` / `session_id` ≥ 8 字符）；
   - 优先级 2：前缀哈希派生（`CC_SESSION_STRATEGY=prefix`，默认）：`sha256(system + tools + 首条用户消息)`，同对话跨轮稳定；
   - 优先级 3：Per-Key 轮换（`CC_SESSION_STRATEGY=key`，原版 12h + 1h 抖动轮换）。
5. **Thinking 思考回传与签名**：
   - 上游 CC 在思考模式下强制校验历史中的思考过程，缺失会导致 `502 (The reasoning_content in the thinking mode must be passed back to the API)`；
   - 历史 assistant 思考块默认以 `{type: "reasoning", text: ...}` 格式回传上游（可通过 `CC_ASSISTANT_REASONING=0` 显式禁用），且严格遵循 CC CLI 抓包顺序 `[reasoning, text, tool-call]`；
   - Thinking 思考签名采用确定性伪造（`0x12` + sha256 前缀）满足下游客户端浅校验；上游不接受也不回传签名，历史记录中的签名发往上游时需过滤。
6. **安全丢弃字段**（上游无对应能力，对齐原版行为）：
   - Anthropic 入站：`stop_sequences`、`top_k`、`metadata.user_id`、`tool_result.is_error`。
   - Responses/Chat 入站追加：`truncation`、`include`、`background`、`service_tier`、`prompt_cache_key`、`safety_identifier`、`user`、`metadata`、`text`（format/verbosity）、`top_logprobs`、`stream_options`、`logprobs`、`response_format`/`text.format`（结构化输出，安全丢弃不转换）；Chat 追加 `stop`（不映射、不落 `out.StopSequences`）、`frequency_penalty`、`presence_penalty`、`logit_bias`、`store`、`prediction`、`extra_body`（无类型字段由 `types.ChatIgnoredFields(raw)` 探测存在性后留痕）；Chat 的 `n>1` 直接 400。
   - `reasoning.encrypted_content` 丢弃**不留痕**（Codex 每轮必发，留痕会淹没日志）。
   - 所有丢弃必须留痕（`info:` 前缀为良性观测，其余 warn），两条入站路径口径一致（空 tool_result 归一化为 `""`、effort 档位收窄 `minimal→low`、`xhigh/max→high`）。
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

1. **缓存正常表现**：$C \approx P$（50% 比例）正是第二步全量命中缓存的正常表现，非缺陷。
2. **断点合成位置**：从**信封整体末尾**向前回扫最后一个 `text` part（勿改回只扫 `user` 消息 —— Agent 交互链尾部常为 `role: "tool"`，只扫 user 会停滞在首条人类消息）。
3. **客户端标记优先**：
   - `CC_CACHE_MARKERS=respect`（默认）：优先透传客户端 part 级标记，仅在缺失时进行末尾合成。
   - `CC_CACHE_MARKERS=replace`：剥离客户端标记并强制末尾合成（仅用于 A/B 诊断）。
4. **断点可观测性**：
   - 透传日志：`info: ... marker(s) present in envelope`
   - 合成日志：`WARN: no part-level cache_control found`

---

## 六、 环境与发版流水线

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

---

## 八、 智能体协作指引

- **主动发版 Tag 提醒规范**：
  - **触发时机**：当协助完成功能新增、Bug 修复或准备交付部署时。
  - **行动要求**：智能体须主动提示用户提交 Tag，明确说明 `:latest` 镜像与多平台交叉编译二进制均依赖 Tag 触发构建，并附带具体命令：`git tag vX.Y.Z && git push origin vX.Y.Z`。

