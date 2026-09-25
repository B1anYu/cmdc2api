package translate

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------- clipWarnValue：嵌进留痕的客户端文本必须有界 ----------

func TestClipWarnValue(t *testing.T) {
	// 短值原样通过（既有测试钉住的文案不受影响）
	if got := clipWarnValue("short"); got != "short" {
		t.Errorf("短值被改动: %q", got)
	}
	// 恰好等于上限不截断
	exact := strings.Repeat("a", warnValueMax)
	if got := clipWarnValue(exact); got != exact {
		t.Errorf("恰好等于上限不该截断: len=%d", len(got))
	}
	// 超限：保留前缀 + 注明原始长度
	long := strings.Repeat("a", warnValueMax+500)
	got := clipWarnValue(long)
	if !strings.HasPrefix(got, exact) {
		t.Errorf("前缀被破坏")
	}
	if !strings.Contains(got, fmt.Sprintf("truncated, %d bytes total", len(long))) {
		t.Errorf("缺少原始长度标注: %q", got)
	}
	if len(got) > warnValueMax+64 {
		t.Errorf("截断后仍过长: %d", len(got))
	}
	// 多字节字符不得被切半
	cjk := strings.Repeat("字", warnValueMax) // 3 字节/字符，必然落在字符中间
	if got := clipWarnValue(cjk); !utf8.ValidString(got) {
		t.Errorf("截断切断了 UTF-8")
	}
}

// 端到端：超长客户端原文进留痕后，单条留痕仍然有界。
// 请求体上限默认 100MB（CC_MAX_BODY_MB），聚合还会把同族明细再复制最多 3 份。
func TestChatToRequest_WarnTextBounded(t *testing.T) {
	// arguments 的**内容**是一串数字：本身是合法 JSON（一个 number），但不是 object，
	// 于是走「合法 JSON 但非对象」那条分支，并把整段原文嵌进留痕。
	big := strings.Repeat("1", 60000)
	raw := fmt.Sprintf(`{"model":"m","messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":"x","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":%q}}]},
		{"role":"tool","tool_call_id":"c1","content":"r"}]}`, big)
	_, warns := mustChatToRequestBody(t, raw)

	found := false
	for _, w := range warns {
		if !strings.Contains(w, "arguments are valid JSON but not an object") {
			continue
		}
		found = true
		if !strings.Contains(w, "truncated") {
			t.Errorf("超长客户端文本未被截断: len=%d", len(w))
		}
		if len(w) > 2048 {
			t.Errorf("留痕仍过长: %d 字节", len(w))
		}
	}
	if !found {
		t.Fatalf("未触发 non-object arguments 留痕: %v", warns)
	}
}

// ---------- chatWarnFamilyKey：折叠粒度 ----------

func TestChatWarnFamilyKey(t *testing.T) {
	const tpl = "unanswered tool_use dropped (no matching tool_result in history): "

	// 带字母的 id：只差数字与字母，仍应归为一族（旧实现折成 toolu_#Ab# / toolu_#Cd# 会分家）
	if a, b := chatWarnFamilyKey(tpl+"toolu_01Ab3"), chatWarnFamilyKey(tpl+"toolu_02Cd7"); a != b {
		t.Errorf("含字母的 id 未被折叠: %q vs %q", a, b)
	}
	// 纯数字 id 仍归为一族
	if a, b := chatWarnFamilyKey(tpl+"call_1"), chatWarnFamilyKey(tpl+"call_2"); a != b {
		t.Errorf("纯数字 id 未被折叠: %q vs %q", a, b)
	}
	// 引号内的工具名不折：含数字的不同工具不得并族
	x := `tool call "t1": arguments are valid JSON but not an object (AAAA); passed through`
	y := `tool call "t2": arguments are valid JSON but not an object (AAAA); passed through`
	if chatWarnFamilyKey(x) == chatWarnFamilyKey(y) {
		t.Errorf("引号内工具名被误折，不同工具并族: %q", chatWarnFamilyKey(x))
	}
	// 同工具、内容不同：键里带着客户端文本，仍分开
	z := `tool call "t1": arguments are valid JSON but not an object (BBBB); passed through`
	if chatWarnFamilyKey(x) == chatWarnFamilyKey(z) {
		t.Errorf("内容不同却同键: %q", chatWarnFamilyKey(x))
	}
	// 引号内内容整体照抄（数字也不折）
	if k := chatWarnFamilyKey(`unknown role "obs9"`); strings.Contains(k, "#") {
		t.Errorf("引号内数字被折: %q", k)
	}
	// 引号外的消息索引仍折（聚合的原始动机场景）
	if a, b := chatWarnFamilyKey("message 0: x"), chatWarnFamilyKey("message 7: x"); a != b {
		t.Errorf("消息索引未被折叠: %q vs %q", a, b)
	}
}

// ---------- 失实文案：无 text part 承载断点 ----------

// 旧文案断言成因是「system/tools 上有标记」，但新增的两个入站协议根本不可能有 cache_control
// 字段，该分支的成因与文案所述无关；且旧文案没说后果（该请求静默失去 part 级断点）。
func TestCacheMarker_NoTextPartWarnIsCauseNeutral(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	req, warns := mustChatToRequestBody(t, raw)
	_, bw := BuildCcRequest(req, BuildOpts{
		Now: time.Now(), NodeVersion: "v22.21.0", WorkingDir: `C:\Users\x\app`, CacheMarkers: "respect",
	})
	warns = append(warns, bw...)

	got := ""
	for _, w := range warns {
		if strings.Contains(w, "no text part in the envelope can carry a cache breakpoint") {
			got = w
		}
	}
	if got == "" {
		t.Fatalf("缺少 no-text-part 留痕: %v", warns)
	}
	if strings.Contains(got, "cache_control present on system/tools") {
		t.Errorf("文案仍断言 system/tools 上有标记: %s", got)
	}
	if !strings.Contains(got, "no part-level breakpoint") {
		t.Errorf("文案未说明后果: %s", got)
	}
}

// ---------- 逐项字段类型错误必须浮出来 ----------

// 旧实现把 item 内字段的类型错误吞成「顶层 input 形状不对」，把客户端引到它明明写对了的
// input 结构上。文案前缀被 internal/server/responses_test.go 依赖，必须保留。
func TestResponsesToRequest_FieldTypeErrorNamesTheField(t *testing.T) {
	err := mustRespinErr(t, `{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":{"a":1}}]}`)
	if !strings.Contains(err.Error(), "arguments") {
		t.Errorf("报错未指向 arguments: %v", err)
	}
	if !strings.Contains(err.Error(), "input must be a string or an array") {
		t.Errorf("既有文案前缀被破坏: %v", err)
	}
}
