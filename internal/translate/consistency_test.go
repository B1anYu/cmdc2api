package translate

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// 本文件是 fix-plan §五 F24 第 4 条要求的「跨路径一致性测试」。
//
// 出发点：三条入站（Anthropic Messages / OpenAI Chat Completions / OpenAI Responses）归一化后
// 都必须流经**同一个** BuildCcRequest，因此同一语义值在三处应当产出同一个 cmdc 信封。
// 单场景 golden 只能钉住「某一条路径的取值」，钉不住「路径之间是否分叉」——F5（缓存断点）与
// F6（effort 透传）都是典型的**路径间**不一致缺陷：任一条路径自己的 golden 都能照常通过。
//
// 因此本文件按「同一语义 × 三条路径」表驱动，每条断言在失败信息里点名是哪两条路径分叉。
// 注意取值的对照基准是**线格式**（经 JSON 往返的 map），而不是 Go 结构体：结构体字段恒在，
// 看不出 omitempty 的效果，而「键是否出现」正是这些字段的契约。

// pathInput 一次跨路径对照的共享语义取值。
type pathInput struct {
	maxTokens int
	effort    string
	topP      *float64
	// cacheControl 只在 Anthropic 入站可表达：OpenAI 两个协议本身没有 cache_control 字段
	//（这正是 F5 要覆盖的差异——代理必须替它们合成断点）。该取值对其余两条路径无意义，被忽略。
	cacheControl bool
}

func floatPtr(v float64) *float64 { return &v }

// inboundPath 一条入站的构造器：产出规范格式（Anthropic 形状）请求。
// 调用方统一再过一次 BuildCcRequest，得到的才是三条路径可比的 cmdc 信封。
type inboundPath struct {
	name  string
	build func(t *testing.T, in pathInput) (*types.Request, []string)
}

// inboundPaths 三条入站。顺序固定：失败信息以 results[0] 为基准，顺序变了日志就不可复现。
var inboundPaths = []inboundPath{
	{name: "anthropic.messages", build: buildAnthropicPath},
	{name: "chat.completions", build: buildChatPath},
	{name: "responses", build: buildResponsesPath},
}

// buildAnthropicPath 一等公民路径：请求体直接就是规范格式。
func buildAnthropicPath(t *testing.T, in pathInput) (*types.Request, []string) {
	t.Helper()
	part := `{"type":"text","text":"hello"}`
	if in.cacheControl {
		// 客户端 part 级断点（含 ttl）：F3 拍板「原样透传、绝不覆写」。
		part = `{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}`
	}
	thinking, topP := "", ""
	if in.effort != "" {
		thinking = fmt.Sprintf(`,"thinking":{"type":"adaptive","effort":%q}`, in.effort)
	}
	if in.topP != nil {
		topP = fmt.Sprintf(`,"top_p":%v`, *in.topP)
	}
	raw := fmt.Sprintf(
		`{"model":"claude-sonnet-4-6","max_tokens":%d%s%s,"messages":[{"role":"user","content":[%s]}]}`,
		in.maxTokens, thinking, topP, part)
	return parseReq(t, raw), nil
}

// buildChatPath Chat Completions 入站（带原始报文，走 handler 的实际入口）。
func buildChatPath(t *testing.T, in pathInput) (*types.Request, []string) {
	t.Helper()
	effort, topP := "", ""
	if in.effort != "" {
		effort = fmt.Sprintf(`,"reasoning_effort":%q`, in.effort)
	}
	if in.topP != nil {
		topP = fmt.Sprintf(`,"top_p":%v`, *in.topP)
	}
	raw := fmt.Sprintf(
		`{"model":"gpt-5","max_tokens":%d%s%s,"messages":[{"role":"user","content":"hello"}]}`,
		in.maxTokens, effort, topP)
	return mustChatToRequestBody(t, raw)
}

// buildResponsesPath Responses 入站（面向 Codex CLI）。
func buildResponsesPath(t *testing.T, in pathInput) (*types.Request, []string) {
	t.Helper()
	reasoning, topP := "", ""
	if in.effort != "" {
		reasoning = fmt.Sprintf(`,"reasoning":{"effort":%q}`, in.effort)
	}
	if in.topP != nil {
		topP = fmt.Sprintf(`,"top_p":%v`, *in.topP)
	}
	raw := fmt.Sprintf(`{"model":"gpt-5","max_output_tokens":%d%s%s,"input":"hello"}`,
		in.maxTokens, reasoning, topP)
	req, _, warns := respin(t, raw)
	return req, warns
}

// pathResult 一条路径的对照产物：信封 + 该路径全链路的留痕（入站层 + BuildCcRequest）。
type pathResult struct {
	name  string
	cc    *types.CcRequest
	warns []string
}

// crossPathRun 把同一语义请求喂给三条入站，各自再过一次 BuildCcRequest。
func crossPathRun(t *testing.T, in pathInput) []pathResult {
	t.Helper()
	out := make([]pathResult, 0, len(inboundPaths))
	for _, p := range inboundPaths {
		req, warns := p.build(t, in)
		cc, buildWarns := BuildCcRequest(req, testOpts())
		out = append(out, pathResult{name: p.name, cc: cc, warns: append(warns, buildWarns...)})
	}
	return out
}

// crossPathSame 断言三条入站在 extract 上取值一致，返回该取值。
// 失败信息点名是哪两条路径分叉——跨路径断言最容易写成「三条都断言了、却不知道谁跟谁不一致」，
// 不点名就得把三条路径重跑一遍才能定位。
func crossPathSame(t *testing.T, results []pathResult, what string, extract func(*types.CcRequest) string) string {
	t.Helper()
	want := extract(results[0].cc)
	for _, r := range results[1:] {
		if got := extract(r.cc); got != want {
			t.Errorf("跨路径不一致（%s）：%s = %s，但 %s = %s",
				what, results[0].name, want, r.name, got)
		}
	}
	return want
}

// envelopeParams 把信封经 JSON 往返解成通用 map 的 params 对象。
// 断言基准是线格式字段存在性（而非 Go 结构体）：结构体上字段恒在，看不出 omitempty 的效果。
func envelopeParams(t *testing.T, cc *types.CcRequest) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, cc)), &raw); err != nil {
		t.Fatalf("信封不是合法 JSON: %v", err)
	}
	params, ok := raw["params"].(map[string]any)
	if !ok {
		t.Fatalf("信封缺少 params 对象: %v", raw)
	}
	return params
}

// crossPathParamsKeys 返回信封 params 的键集合（用于「键必须缺席」类断言）。
func crossPathParamsKeys(t *testing.T, cc *types.CcRequest) string {
	t.Helper()
	keys := make([]string, 0, 8)
	for k := range envelopeParams(t, cc) {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

// ---------- 1. max_tokens ----------

// TestCrossPathEnvelope_MaxTokens F4：max_tokens 的默认值/上限由 BuildCcRequest 统一裁决
// （Chat 侧 ≤0 不在入站兜底、Responses 侧取 max_output_tokens），三条入站必须给出同一个值，
// 且钳制留痕一条不多、一条不少。
func TestCrossPathEnvelope_MaxTokens(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		want      string
		wantClamp bool
	}{
		{"5000 未触上限", 5000, "5000", false},
		{"300000 触上限被钳制", 300000, "200000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := crossPathRun(t, pathInput{maxTokens: tc.maxTokens})
			got := crossPathSame(t, results, "params.max_tokens", func(cc *types.CcRequest) string {
				return strconv.Itoa(cc.Params.MaxTokens)
			})
			if got != tc.want {
				t.Errorf("params.max_tokens = %s, want %s", got, tc.want)
			}
			for _, r := range results {
				n := countWarns(r.warns, "clamped to 200000")
				switch {
				case tc.wantClamp && n != 1:
					t.Errorf("%s: 钳制留痕 %d 条, want 1（钳制必须留痕且只留一条）；warns=%v",
						r.name, n, r.warns)
				case !tc.wantClamp && n != 0:
					t.Errorf("%s: 未发生钳制却出现 %d 条钳制留痕；warns=%v", r.name, n, r.warns)
				}
			}
		})
	}
}

// ---------- 2. thinking / effort 档位 ----------

// TestCrossPathEnvelope_ReasoningEffort F6：max 与 xhigh 必须**原样**透传。
// 「三条一致」之外还必须钉住取值本身——一致地把 max 收窄成 high（F6 之前的实现）同样是缺陷，
// 只断言一致性会放过它。
func TestCrossPathEnvelope_ReasoningEffort(t *testing.T) {
	for _, effort := range []string{"max", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			results := crossPathRun(t, pathInput{maxTokens: 1000, effort: effort})
			got := crossPathSame(t, results, "params.reasoning_effort", func(cc *types.CcRequest) string {
				return cc.Params.ReasoningEffort
			})
			if got != effort {
				t.Errorf("params.reasoning_effort = %q, want %q（F6：档位原样透传，不收窄）", got, effort)
			}
		})
	}
}

// ---------- 3. 缓存断点 ----------

// TestCrossPathEnvelope_CacheBreakpoint F5 + F3：三条路径的信封尾部都应有 part 级断点。
// Anthropic 带入的客户端断点（含 ttl）原样透传、不被覆写；Chat/Responses 协议没有 cache_control
// 字段，断点只能由代理在信封末尾合成，且必须有留痕。
func TestCrossPathEnvelope_CacheBreakpoint(t *testing.T) {
	t.Run("客户端断点原样透传 + 缺断点者由代理补齐", func(t *testing.T) {
		results := crossPathRun(t, pathInput{maxTokens: 1000, cacheControl: true})
		got := crossPathSame(t, results, "信封尾部 part 级断点的 type", func(cc *types.CcRequest) string {
			if m := envelopeTailMarker(cc); m != nil {
				return m.Type
			}
			return "<无断点>"
		})
		if got != "ephemeral" {
			t.Errorf("信封尾部断点 type = %s, want ephemeral（三条路径都必须有断点）", got)
		}
		for _, r := range results {
			m := envelopeTailMarker(r.cc)
			if m == nil {
				t.Fatalf("%s: 信封尾部没有 part 级断点", r.name)
			}
			if r.name == "anthropic.messages" {
				// F3：客户端标记原样透传——ttl 既不能被剥掉，也不能被代理改写成别的时长。
				if m.TTL != "1h" {
					t.Errorf("%s: 客户端 ttl 被覆写或丢失: %+v（F3 要求原样透传）", r.name, m)
				}
				if n := countWarns(r.warns, "synthesized a breakpoint"); n != 0 {
					t.Errorf("%s: 客户端已带断点却出现 %d 条合成留痕；warns=%v", r.name, n, r.warns)
				}
				continue
			}
			// F5：合成断点只有 {type:ephemeral}，不得凭空注入 ttl（会改变上游缓存写入成本）。
			if m.TTL != "" {
				t.Errorf("%s: 合成断点不应带 ttl: %+v", r.name, m)
			}
			if n := countWarns(r.warns, "synthesized a breakpoint"); n != 1 {
				t.Errorf("%s: 合成留痕 %d 条, want 1；warns=%v", r.name, n, r.warns)
			}
		}
	})

	// 协议差异，刻意如此而非缺陷：Anthropic 客户端不带断点时，代理无从判断「这条对话是否值得缓存」，
	// 因此不合成；而 Chat/Responses 协议**根本没有** cache_control 字段，不合成等于这两个端点
	// 永远拿不到断点（F5 的原始缺陷）。这里把差异显式钉住，避免以后被当成不一致而「修平」。
	t.Run("Anthropic 无客户端断点时不合成的协议差异", func(t *testing.T) {
		results := crossPathRun(t, pathInput{maxTokens: 1000})
		for _, r := range results {
			m := envelopeTailMarker(r.cc)
			if r.name == "anthropic.messages" {
				if m != nil {
					t.Errorf("%s: 未声明 cache_control 却合成了断点（与 F5 的入站语义不符）: %+v", r.name, m)
				}
				continue
			}
			if m == nil {
				t.Errorf("%s: 协议无 cache_control 字段，代理必须合成断点（F5）", r.name)
			}
		}
	})
}

// ---------- 4. top_p ----------

// TestCrossPathEnvelope_TopPDropped F14：top_p 在整个代理里被有意静默丢弃——
// 三条入站的信封 params 都不得出现 top_p 键，且**不留痕**（这是「所有丢弃必须留痕」规约的
// 显式例外，所以这里反过来钉住「没有 warn」，防止有人按默认规约给它补上一条）。
// 出站侧同理：回显一个从未到达上游的参数等于向客户端谎报它已生效。
func TestCrossPathEnvelope_TopPDropped(t *testing.T) {
	results := crossPathRun(t, pathInput{maxTokens: 1000, topP: floatPtr(0.9)})

	got := crossPathSame(t, results, "信封 params 是否出现 top_p 键", func(cc *types.CcRequest) string {
		if _, ok := envelopeParams(t, cc)["top_p"]; ok {
			return "present"
		}
		return "absent"
	})
	if got != "absent" {
		t.Errorf("信封 params 出现了 top_p 键（F14 要求丢弃）: %s", crossPathParamsKeys(t, results[0].cc))
	}
	for _, r := range results {
		for _, w := range r.warns {
			if strings.Contains(w, "top_p") {
				t.Errorf("%s: top_p 丢弃不应留痕（F14 的显式例外），却出现: %q", r.name, w)
			}
		}
	}

	// 出站响应体：形态由出站协议决定，与入站路径无关，故三条路径共用一个断言。
	evs := respStreamEvents(t,
		`{"type":"text-delta","text":"hi"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":1}}`)
	chat := NewChatAggregator("gpt-5", "chatcmpl-top_p")
	for _, ev := range evs {
		chat.Feed(ev)
	}
	assertNoTopPEcho(t, "chat.completions 响应体", chat.Completion())

	agg := NewResponsesAggregator("gpt-5", "resp_top_p", nil)
	for _, ev := range evs {
		agg.Feed(ev)
	}
	assertNoTopPEcho(t, "responses 响应体", agg.Result())
}

// assertNoTopPEcho 断言响应体的线格式里没有 top_p 键（含 null：F14 之前它是「键恒在、值为 null」）。
func assertNoTopPEcho(t *testing.T, what string, body any) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, body)), &m); err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", what, err)
	}
	if v, ok := m["top_p"]; ok {
		t.Errorf("%s 回显了 top_p=%v（该参数从未到达上游）", what, v)
	}
}
