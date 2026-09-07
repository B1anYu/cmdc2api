# cmdc-proxy → Go 重写 · 开发上下文（2026-09-07 调研快照）

> 本文件是把 VPS 上的调研结论打包，供在**本地电脑**开发 Go 版 cmdc-proxy 用。源码级坐标均基于上游 master（`fcdb56a`，v1.0.0 之后的 head）。所有 `proxy.mjs:NNN` 行号指向该版本。

---

## 0. 一句话任务

把 [MAXeaglet/commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy)（Node.js 单文件，~2000 行）用 Go 重写，目标：**直转 Anthropic→cmdc（绕开 OpenAI chat 中间层）**，补上它的保真度缺口，静态二进制部署。

## 1. 决策背景（为什么重写）

| 动机 | 说明 |
|---|---|
| 保真度 | 原版两阶段转换（Anthropic→OpenAI→cmdc）丢字段：图片、thinking 块、cache_control、签名 |
| 掌控协议层 | 伪装层（指纹/lifecycle/信封）是它真正的资产，重写=吃透 |
| 资源 | 静态二进制 vs node:22-alpine；node 容器实测空闲仅 18MiB，省的不多，加分项 |
| 简历 | 显式理由之一 |
| 停用 | VPS 上 node 版已 `docker compose down`（2026-09-07）；compose/README 留档备查 |

**风险红线（必读）**：cmdc-proxy 属「伪装官方客户端」反代。zcode-proxy（同类）已在 2026-09-07 因账号被上游风控封禁而停用（VPS 知识库第十二节）。**测试/使用别用主号，先小号+低频验证账号存活。**

## 2. 上游与本地参考

- 上游仓库：`gh repo clone MAXeaglet/commandcode-proxy`（MIT）
- VPS 上的完整 clone（含全 git 历史，`git fetch --unshallow`）：`/tmp/cmp/commandcode-proxy` —— **重启会清，别依赖**
- VPS 上 sub2api 源码（保真度对标，见 §5）：`/tmp/cmp/sub2api`（sparse，仅 `backend/internal/pkg/apicompat`）—— 同样重启清，本地用 `gh repo clone Wei-Shaw/sub2api` 取
- 上游当前状态：
  - GHCR 镜像只发了 `v1.0.0`（tag `0e76182`）；master（`fcdb56a`）多 `zdr` 特性未发版
  - 关键修复 PR #5 卡着未合：`https://github.com/MAXeaglet/commandcode-proxy/pull/5`（缓存修复，**`mergeable:false` 有冲突，v1.0.0 不含**）
  - 相关 issue：#4（thinking 不回传，已修）、#17（无 system 时 7.5K 额外 cache 计费）

## 3. 上游协议全景（proxy.mjs 坐标）

### 3.1 伪装层（重写必须照抄的部分 —— 这就是它「能跑」的原因）

| 组件 | 位置 | 要点 |
|---|---|---|
| 设备指纹 | `generateFingerprint` :80 | `FINGERPRINT_CPUS/MEMS/TZS`（:54-78 表）+ 随机 2~5 个 MAC；首次运行生成，**写回 config.json** |
| lifecycle 预请求 | `ensureInitialized` :243 | 并行 `POST /alpha/fingerprint/record` + `POST /alpha/lifecycle-events`（`eventType: cli_session_exists`）；每 key 节流，初始后每 8h+2h 抖动再刷 |
| CC 版本号 | `refreshCCVersion` :128 | 每 24h 从 **npm registry** 拉官方 CLI 版本号 → 写进 `x-command-code-version` 头；fallback `0.32.3` |
| traceparent | `generateTraceparent` :367 | OTel 格式 |
| 会话 | `sessionStore` :172 / `ensureSession` :174 / `getSessionId` :204 | **按 apiKey 隔离**，12h+1h 抖动 |
| project-slug | `fakeProjectSlug` :342 | 按 sessionId 派生 |
| key 鉴权 | `getApiKey` :812 | 入站 `Authorization: Bearer user_xxx` 或 `x-api-key`，正则 `user_[a-zA-Z0-9_-]+`；拿不到 401；**key 就是 cmdc 账号 key，原样透传上游** |
| 错误映射 | `mapCcError` :718 + `CC_STATUS_MAP` :705 | 上游 4xx→4xx，零输出/超时→429 带 Retry-After（SDK 自动重试） |
| zdr | `CFG.zdr` / `CMD_ZDR` | 可选 `x-cmd-zdr: 1` 仅走 ZDR 路由 |

### 3.2 上游端点（apiBase 默认 `https://api.commandcode.ai`）

| 端点 | 用途 | 头 |
|---|---|---|
| `POST /alpha/generate` | 主推理（:831） | `Authorization: Bearer {key}`、`x-cli-environment: production`、`x-command-code-version`、`x-session-id`、`x-co-flag: false`、`x-taste-learning: false`、`x-project-slug`、`traceparent`、（`x-cmd-zdr`） |
| `POST /alpha/fingerprint/record` | 预请求（:260） | 同上（少 session/traceparent） |
| `POST /alpha/lifecycle-events` | 预请求（:270） | 同上 |
| `GET /provider/v1/models` | 模型列表（:1955） | 需要 key |

### 3.3 cmdc 信封格式（`buildCcRequest` :387 构造）

```jsonc
{
  "config": { "workingDir":".","date":"...","environment":"{platform}-{arch}","structure":[],
              "isGitRepo":false,"currentBranch":"","mainBranch":"","gitStatus":"","recentCommits":[] },
  "memory": null, "taste": null, "skills": "",
  "permissionMode": "standard",
  "params": {
    "model": "deepseek/deepseek-v4-flash",   // 默认
    "messages": [  // 注意：content 是块数组
      { "role":"user","content":[{"type":"text","text":"..."}] },
      { "role":"assistant","content":[{"type":"text","text":"..."},
                                      {"type":"tool-call","toolCallId":"...","toolName":"...","input":{...}}] },
      { "role":"tool","content":[{"type":"tool-result","toolCallId":"...","toolName":"...",
                                  "output":{"type":"text","value":"..."}}] }
    ],
    "system": "必须是字符串，数组会被上游直接拒（400 Validation error）",
    "max_tokens": "min(请求值||64000, 200000)",
    "stream": true,   // cmdc 总是流式
    // 可选：temperature / reasoning_effort / tools[].input_schema / tool_choice{type:auto|none|any|tool,name}
    // 图片块：{"type":"image","image":"data:image/...;base64,..."}
  }
}
```

- 块类型：`text` / `image` / `tool-call`（toolCallId+toolName+input）/ `tool-result`（toolCallId+toolName+output{type:text,value}）
- tools 用 `input_schema`（Anthropic 命名）；tool_choice 是 Anthropic 风格 `{type:tool,name}` —— **cmdc 是 messages 风味，不是 OpenAI chat**

### 3.4 上游响应（NDJSON 流式事件）

`start`/`start-step`/`text-start`/`reasoning-start`（信号）→ `reasoning-delta`（thinking 文本）→ `text-delta` → `tool-call`（toolCallId/toolName/input）→ `finish-step`/`finish`（带 `finishReason` + `totalUsage`/`usage`）→ `error`；另有 `reasoning-end`/`provider-metadata`/`tool-input-*`/`text-end`/`tool-error`（静默忽略）。

usage 形状：`inputTokens`/`outputTokens`/`cachedInputTokens`/`inputTokenDetails.cacheWriteTokens`（`normalizeUsage` :686）。

## 4. 原版转换的两阶段结构（要绕开的东西）

```
Anthropic  /v1/messages   ──convertAnthropicToOpenAI──►  OpenAI chat 中间格式  ──buildCcRequest──►  cmdc
OpenAI     /v1/chat/completions ────────────────────────────────► (直通)        ──buildCcRequest──►  cmdc
```

- `convertAnthropicToOpenAI` :1280 —— Anthropic→OpenAI chat（丢字段的重灾区）
- `buildCcRequest` :387 —— 唯一的 cmdc 信封构造器（OpenAI chat 形状喂入）
- `createAnthropicSseTranslator` :1419 —— cmdc NDJSON→Anthropic SSE
- `createSseTranslator` :548 —— cmdc NDJSON→OpenAI SSE

**设计理由**：工程复用（一个 body 构造器，两入站归一到 OpenAI chat），不是协议要求。cmdc 本身是 messages 风味，Anthropic 离它最近——绕 OpenAI chat 一趟纯亏。

## 5. 保真度缺口清单（Go 重写必须修的）

| 缺口 | 原版表现 | 重写目标 |
|---|---|---|
| **cache_control 请求断点** | Anthropic 路径全丢（只认 OpenAI 的 `prompt_cache_key` 参数 :473）；PR #5 证实 cache_read 恒 0 | 直转时保留 system/content/tools 上的 cache_control 标记 |
| **Anthropic 用户消息图片** | `convertAnthropicToOpenAI` 只处理 text/tool_result，image 块**静默丢弃**（:1331-1339） | image→cmdc `{type:image}` |
| **tool_result 图片** | 只 `c.text` 拼接（:1344-1346） | 保留图片 |
| **前轮 thinking 块** | assistant 只留 text/tool_use，thinking 块丢弃（:1307-1322） | 多轮思考链保留 |
| **响应 thinking 签名** | `fakeThinkingSignature` :1243（sha256 派生+0x12 前缀），假的 | 尽量真实/至少可验证 |
| **thinking budget→effort** | 阈值近似（≥10000→high…<2000→low，:1404-1408） | 精确映射 |
| **disable_parallel_tool_use** | 不映射 | 映射 |
| **usage 精确性** | tool 调用 output token 按 +20 估算（:1552） | 精确记账 |
| **system 结构** | 数组拼字符串丢结构（:1286-1291） | 保留块结构 + cache_control |

**保真度对标（sub2api）**：`Wei-Shaw/sub2api` → `backend/internal/pkg/apicompat/anthropic_to_responses.go`（直接转换、system→developer item、图片→input_image、thinking+signature→encrypted_content）。这是「成熟转换层」的参考标准。注意 sub2api 是转 OpenAI Responses（账号池），cmdc 是转私有格式——保真度目标是「对齐这些能力」，不是抄它的目标格式。

## 6. 设计草案

- **直转**：`convertAnthropicToCmdc(req)` 直接从 Anthropic 请求构造 cmdc 信封（绕开 OpenAI chat 中间层）；`convertOpenAIToCmdc` 单独一条。
- **响应**：cmdc NDJSON → Anthropic SSE / OpenAI SSE 两个翻译器（参考 :548/:1419）。
- **伪装层**：照抄 §3.1（指纹、lifecycle、版本号、会话、traceparent、鉴权、错误映射、zdr）——这是协议知识，不是实现智慧，MIT 许可可参考。
- **配置**：env + config（API base、PORT、log、CC_MAX_BODY_MB 100MB、model refresh 5min、zdr）。
- **运行**：静态二进制，loopback 绑定，可挂 axonhub_default 网络。`/v1/messages` + `/v1/chat/completions` + `/v1/models` + `/health`。
- **先修哪几个**：cache_control 保留 > 图片（用户/工具）> thinking 链 + 签名 > usage 精确。

## 7. 验收标准

1. `/v1/messages` 流式 + 非流式，工具调用（tool-call/tool-result 往返）正常
2. cache_control 断点保留 → 连续两轮同前缀请求 `cache_read_input_tokens > 0`（原版恒 0）
3. 图片（Anthropic 入站）上游能看到
4. 多轮 thinking 保留 + 签名合法
5. `user_` key 鉴权、401/429/502 错误语义对齐
6. 设备指纹/lifecycle 预请求发出、会话 12h 隔离生效
7. 小号 + 低频实弹跑 1-2 天，账号存活

## 8. 附：VPS 部署上下文（重写完可再部署）

- 端口 `127.0.0.1:3050`（loopback），容器名 `commandcode-proxy`，挂 `axonhub_default` 网络
- 接 axonhub：渠道类型 `anthropic`、base_url `http://commandcode-proxy:3050`、渠道 key=cmdc `user_` key
- 参考原 node 版 compose：`/tmp/cmp/commandcode-proxy/docker-compose.yml`（VPS 上，重启清）或 `~/ai-stack/commandcode-proxy/docker-compose.yml`（留档）
