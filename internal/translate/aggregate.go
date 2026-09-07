package translate

import (
	"encoding/json"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// Aggregator 把 Anthropic SSE 事件流折叠为非流式 Message。
// 与流式路径消费同一事件序列，保证两种出站形态语义一致。
type Aggregator struct {
	msg       types.Message
	inputJSON map[int]*stringsBuilderAlias
	curIdx    int
}

// stringsBuilderAlias 避免 import strings 仅为此处。
type stringsBuilderAlias struct {
	b []byte
}

func (s *stringsBuilderAlias) WriteString(str string) { s.b = append(s.b, str...) }
func (s *stringsBuilderAlias) String() string         { return string(s.b) }

func NewAggregator() *Aggregator {
	return &Aggregator{
		msg:       types.Message{Type: "message", Role: "assistant", Content: []types.ContentBlock{}},
		inputJSON: make(map[int]*stringsBuilderAlias),
	}
}

func (a *Aggregator) Feed(ev types.StreamEvent) {
	switch d := ev.Data.(type) {
	case types.MessageStartEvent:
		a.msg.ID = d.Message.ID
		a.msg.Model = d.Message.Model

	case types.ContentBlockStartEvent:
		switch b := d.ContentBlock.(type) {
		case types.TextBlockStart:
			a.msg.Content = append(a.msg.Content, types.ContentBlock{Type: "text"})
		case types.ThinkingBlockStart:
			a.msg.Content = append(a.msg.Content, types.ContentBlock{Type: "thinking"})
		case types.ToolUseBlockStart:
			a.msg.Content = append(a.msg.Content, types.ContentBlock{
				Type: "tool_use", ID: b.ID, Name: b.Name, Input: json.RawMessage("{}"),
			})
			a.inputJSON[d.Index] = &stringsBuilderAlias{}
		}
		a.curIdx = d.Index

	case types.ContentBlockDeltaEvent:
		if len(a.msg.Content) == 0 {
			break
		}
		blk := &a.msg.Content[len(a.msg.Content)-1]
		switch dd := d.Delta.(type) {
		case types.TextDelta:
			blk.Text += dd.Text
		case types.ThinkingDelta:
			blk.Thinking += dd.Thinking
		case types.SignatureDelta:
			blk.Signature = dd.Signature
		case types.InputJSONDelta:
			if b, ok := a.inputJSON[d.Index]; ok {
				b.WriteString(dd.PartialJSON)
			}
		}

	case types.MessageDeltaEvent:
		a.msg.StopReason = d.Delta.StopReason
		a.msg.Usage = types.Usage{
			InputTokens:              d.Usage.InputTokens,
			OutputTokens:             d.Usage.OutputTokens,
			CacheReadInputTokens:     d.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: derefInt(d.Usage.CacheCreationInputTokens),
		}

	case types.ErrorEvent:
		// 错误路径由 handler 决定出站形态，这里不吞事件
	}
}

// Message 产出最终消息。tool_use 的 input 从累积的 partial_json 解析，
// 非法 JSON（上游截断）兜底为 {}，避免毒历史进入下一轮。
func (a *Aggregator) Message() *types.Message {
	for idx, b := range a.inputJSON {
		if idx < 0 || idx >= len(a.msg.Content) {
			continue
		}
		raw := json.RawMessage(b.String())
		if len(raw) == 0 || !json.Valid(raw) {
			raw = json.RawMessage("{}")
		}
		a.msg.Content[idx].Input = raw
	}
	if a.msg.Content == nil {
		a.msg.Content = []types.ContentBlock{}
	}
	return &a.msg
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
