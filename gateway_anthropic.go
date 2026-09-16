// gateway_anthropic.go 鈥斺€?Anthropic 鍏煎闈細POST /v1/messages
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type anthRequest struct {
	Model     string          `json:"model"`
	System    json.RawMessage `json:"system"`
	Messages  []anthMessage   `json:"messages"`
	Stream    bool            `json:"stream"`
	MaxTokens int             `json:"max_tokens"`
	Tools     []toolDef       `json:"tools"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func anthSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// anthContentToText锛歝ontent 鍙兘鏄瓧绗︿覆锛屼篃鍙兘鏄?[{type:"text"|"tool_result"|...,鈥]
func anthContentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "tool_use":
			b.WriteString("[tool_use " + p.Name + " " + string(p.Input) + "]")
		case "tool_result":
			b.WriteString("[tool_result " + anthContentToText(p.Content) + "]")
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (s *session) handleMessages(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, 400, "invalid_request_error", err.Error())
		return
	}
	logRequestSummary(r.URL.Path, rawBody)
	var req anthRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeErr(w, 400, "invalid_request_error", "璇锋眰浣撲笉鏄悎娉?JSON: "+err.Error())
		return
	}
	msgs := make([]chatMsg, 0, len(req.Messages)+1)
	if sys := anthSystemText(req.System); strings.TrimSpace(sys) != "" {
		msgs = append(msgs, chatMsg{Role: "system", Content: sys})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, chatMsg{Role: m.Role, Content: anthContentToText(m.Content)})
	}
	system, prompt, hist, err := splitConversation(msgs)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", err.Error())
		return
	}
	model, effort := parseModel(req.Model)
	tr := turnRequest{Prompt: prompt, System: system, History: hist,
		Model: model, Effort: effort, Tools: req.Tools}
	logRawTools(req.Tools)

	id := newID("msg")
	pt, ct := usageEstimate(prompt+system, "")
	usage := func(out int) map[string]any {
		return map[string]any{"input_tokens": pt, "output_tokens": out}
	}
	if ct < 0 {
		ct = 0
	}

	if !req.Stream {
		content := []any{}
		var text string
		err := s.runTurn(r.Context(), tr, func(ev turnEvent) {
			switch ev.Kind {
			case "tool":
				var input any = map[string]any{}
				_ = json.Unmarshal([]byte(pick(ev.Args, "{}")), &input)
				content = append(content, map[string]any{
					"type": "tool_use", "id": toolUseID(ev), "name": ev.Name, "input": input})
			case "text":
				text += ev.Text
			case "error":
				text = "[" + ev.Text + "]"
			}
		})
		if err != nil {
			writeErr(w, 502, "api_error", err.Error())
			return
		}
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		_, outTok := usageEstimate("", text)
		stop := "end_turn"
		if len(content) > 0 && localTools {
			// 妗ユ帴妯″紡锛歵ool_use 闇€瑕佽皟鐢ㄦ柟鎵ц锛宻top_reason 蹇呴』鏄?tool_use
			stop = "tool_use"
		}
		writeJSON(w, 200, map[string]any{
			"id": id, "type": "message", "role": "assistant",
			"model": pick(req.Model, defaultModel), "content": content,
			"stop_reason": stop, "stop_sequence": nil, "usage": usage(outTok),
		})
		return
	}

	f := sseHeaders(w)
	ev := func(name string, payload map[string]any) {
		payload["type"] = name
		sseSend(w, f, name, payload)
	}
	ev("message_start", map[string]any{"message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": pick(req.Model, defaultModel),
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage(0)}})
	ev("ping", map[string]any{})

	block := 0
	var text string
	err = s.runTurn(r.Context(), tr, func(e turnEvent) {
		switch e.Kind {
		case "tool":
			idx := block
			block++
			ev("content_block_start", map[string]any{"index": idx, "content_block": map[string]any{
				"type": "tool_use", "id": toolUseID(e), "name": e.Name, "input": map[string]any{}}})
			ev("content_block_delta", map[string]any{"index": idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": pick(e.Args, "{}")}})
			ev("content_block_stop", map[string]any{"index": idx})
		case "text":
			text += e.Text
		case "error":
			text = "[" + e.Text + "]"
		}
	})
	if err != nil {
		ev("error", map[string]any{"error": map[string]any{
			"type": "api_error", "message": err.Error()}})
		return
	}

	idx := block
	ev("content_block_start", map[string]any{"index": idx,
		"content_block": map[string]any{"type": "text", "text": ""}})
	if text != "" {
		ev("content_block_delta", map[string]any{"index": idx,
			"delta": map[string]any{"type": "text_delta", "text": text}})
	}
	ev("content_block_stop", map[string]any{"index": idx})
	_, outTok := usageEstimate("", text)
	stop := "end_turn"
	if block > 0 && localTools {
		stop = "tool_use"
	}
	ev("message_delta", map[string]any{"delta": map[string]any{
		"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": outTok}})
	ev("message_stop", map[string]any{})
}

// toolUseID锛欰nthropic 椋庢牸鐨勫伐鍏疯皟鐢?id
func toolUseID(ev turnEvent) string {
	id := strings.TrimPrefix(pick(ev.ID, newID("toolu")), "call_")
	if id == "" {
		id = newID("toolu")
	}
	return "toolu_" + id
}
