# cmdc2api 项目规则与开发指引

## 一、 项目定位与参照

- **项目目标**：Anthropic Messages → cmdc 单跳直转反向代理（Go 编写，零第三方依赖，静态二进制部署）。
- **当前版本**：`v0.1.2`（2026-09-09）。
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
2. **单跳直转**：Anthropic 协议 → cmdc 信封一步完成，**坚决不引入 OpenAI Chat 中间层**，避免字段与类型丢失。
3. **会话亲和优先级**：
   - 优先级 1：显式 Session Header（`x-session-id` / `x-claude-code-session-id` / `session_id` ≥ 8 字符）；
   - 优先级 2：前缀哈希派生（`CC_SESSION_STRATEGY=prefix`，默认）：`sha256(system + tools + 首条用户消息)`，同对话跨轮稳定；
   - 优先级 3：Per-Key 轮换（`CC_SESSION_STRATEGY=key`，原版 12h + 1h 抖动轮换）。
4. **Thinking 思考签名**：采用确定性伪造（`0x12` + sha256 前缀）满足下游客户端浅校验；上游不回传签名，历史记录中的签名发往上游时需过滤。
5. **安全丢弃字段**（上游无对应能力，对齐原版行为）：
   - `stop_sequences`、`top_k`、`metadata.user_id`、`tool_result.is_error`。
6. **客户端伪装自洽性**：
   - 指纹/Lifecycle/信封 Environment/WorkingDir 全套对齐 Windows x64。
   - 出站 Transport 强制 `HTTP/1.1` 且置空 `User-Agent`（Go 中 `h.Set("User-Agent", "")` 可抑制默认 UA）。
7. **Token Usage 换算**：`input_tokens = max(0, inputTokens − cachedInputTokens)`，严禁改回无脑透传（详见下节）。

---

## 四、 Usage 计费口径（2026-09-09 定案）

### 1. 现象与实测
- 上游 API 返回的 `cachedInputTokens / inputTokens` 比例恒定 $\approx 50\%$。
- 相同请求在上游控制台记录的「总输入」正好为 API `inputTokens` 的一半（例如 80K vs 40K）。

### 2. 模型机理
- cmdc 内部多步循环按步累加 usage（Step 1 全量处理产生 $P$、Step 2 全量命中缓存产生 $P$ 缓存读取）$\rightarrow$ $I \approx 2P, C \approx P$。
- 上游后台按去重 Prompt 计费。

### 3. 定案公式
$$\text{input\_tokens} = \max(0, \text{inputTokens} - \text{cachedInputTokens})$$
- 在「多步求和」与「总量含缓存」两种模型下均等于真实消耗，且与上游后台总输入一致。
- 抽验指标：网关 `input_tokens` 必须与 cmdc 后台总输入统计对齐。

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

---

## 八、 智能体协作指引

- **任务原则**：简单、单点查询由主 Agent 直接执行；子智能体仅用于大规模并行探索或深层调研。
- **模型偏好**：若使用子智能体，非复杂任务优先使用轻量（flash）模型以节省资源。

