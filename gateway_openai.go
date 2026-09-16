// gateway_openai.go —— OpenAI 兼容面：/v1/chat/completions 与 /v1/responses
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

/* ---------------- /v1/chat/completions ---------------- */

type oaiChatRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Stream   bool         `json:"stream"`
	Tools    []toolDef    `json:"tools"`
}

type oaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentToText：兼容 content 为字符串或 [{type:"text",text:"…"}] 两种写法
func contentToText(raw json.RawMessage) string {
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
			if p.Text != "" {
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

func oaiToolCallJSON(ev turnEvent, index int) map[string]any {
	return map[string]any{
		"index": index,
		"id":    pick(ev.ID, newID("call")),
		"type":  "function",
		"function": map[string]any{
			"name":      ev.Name,
			"arguments": pick(ev.Args, "{}"),
		},
	}
}

func (s *session) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, 400, "invalid_request_error", err.Error())
		return
	}
	logRequestSummary(r.URL.Path, rawBody)
	var req oaiChatRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeErr(w, 400, "invalid_request_error", "请求体不是合法 JSON: "+err.Error())
		return
	}
	msgs := make([]chatMsg, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, chatMsg{Role: m.Role, Content: contentToText(m.Content)})
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

	id, created := newID("chatcmpl"), time.Now().Unix()
	usageIn := len(prompt) + len(system)

	if !req.Stream {
		var text string
		var calls []any
		err := s.runTurn(r.Context(), tr, func(ev turnEvent) {
			switch ev.Kind {
			case "text":
				text += ev.Text
			case "tool":
				calls = append(calls, oaiToolCallJSON(ev, len(calls)))
			case "error":
				text = "[" + ev.Text + "]"
			}
		})
		if err != nil {
			writeErr(w, 502, "upstream_error", err.Error())
			return
		}
		msg := map[string]any{"role": "assistant", "content": text}
		finish := "stop"
		if len(calls) > 0 {
			msg["tool_calls"] = calls
			if localTools {
				// 桥接模式下这些调用要由调用方自己执行，必须给明确的 finish_reason
				finish = "tool_calls"
				if strings.TrimSpace(text) == "" {
					msg["content"] = nil
				}
			}
		}
		pt, ct := usageEstimate(strings.Repeat("x", usageIn), text)
		writeJSON(w, 200, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct},
		})
		return
	}

	f := sseHeaders(w)
	chunk := func(delta map[string]any, finish any) {
		sseSend(w, f, "", map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	n := 0
	err = s.runTurn(r.Context(), tr, func(ev turnEvent) {
		switch ev.Kind {
		case "tool":
			chunk(map[string]any{"tool_calls": []any{oaiToolCallJSON(ev, n)}}, nil)
			n++
		case "text":
			if ev.Text != "" {
				chunk(map[string]any{"content": ev.Text}, nil)
			}
		case "error":
			chunk(map[string]any{"content": "[" + ev.Text + "]"}, nil)
		}
	})
	if err != nil {
		sseSend(w, f, "", map[string]any{"error": map[string]any{
			"message": err.Error(), "type": "upstream_error", "code": "upstream_error"}})
	}
	if n > 0 && localTools {
		chunk(map[string]any{}, "tool_calls")
	} else {
		chunk(map[string]any{}, "stop")
	}
	sseRaw(w, f, "data: [DONE]\n\n")
}

/* ---------------- /v1/responses ---------------- */

type respRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions"`
	Stream       bool            `json:"stream"`
	Tools        []toolDef       `json:"tools"`
}

// outputText：function_call_output 的 output 可能是字符串，也可能是 [{type,text}]
func outputText(raw json.RawMessage) string {
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
			if p.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

// parseResponsesInput：input 可以是字符串，也可以是 Responses API 的 item 数组。
// 关键：必须认得 function_call / function_call_output —— Codex 就是靠它把工具执行结果送回来的，
// 不认它就会把结果丢成空消息，模型看不到输出于是把同一条命令反复重下。
func parseResponsesInput(raw json.RawMessage) ([]chatMsg, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []chatMsg{{Role: "user", Content: s}}, nil
	}
	var arr []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("input 既不是字符串也不是消息数组")
	}
	out := make([]chatMsg, 0, len(arr))
	for _, m := range arr {
		switch m.Type {
		case "function_call":
			// 模型自己上一轮请求的工具调用（回灌时带上，让它知道已经调过什么）
			out = append(out, chatMsg{Role: "assistant", Content: fmt.Sprintf(
				"[你请求调用工具 %s，参数 %s]", m.Name, clip(string(m.Arguments), 500))})
		case "function_call_output":
			body := outputText(m.Output)
			out = append(out, chatMsg{Role: "tool", Content: fmt.Sprintf(
				"[tool_result call_id=%s]\n%s", m.CallID, clip(body, 6000))})
		case "reasoning":
			continue
		default:
			out = append(out, chatMsg{Role: pick(m.Role, "user"), Content: contentToText(m.Content)})
		}
	}
	return out, nil
}

func (s *session) handleResponses(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, 400, "invalid_request_error", err.Error())
		return
	}
	logRequestSummary(r.URL.Path, rawBody)
	var req respRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeErr(w, 400, "invalid_request_error", "请求体不是合法 JSON: "+err.Error())
		return
	}
	msgs, err := parseResponsesInput(req.Input)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", err.Error())
		return
	}
	if req.Instructions != "" {
		msgs = append([]chatMsg{{Role: "system", Content: req.Instructions}}, msgs...)
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

	id := newID("resp")
	created := time.Now().Unix()
	base := func(status string, output []any) map[string]any {
		return map[string]any{
			"id": id, "object": "response", "created_at": created,
			"status": status, "model": pick(req.Model, defaultModel),
			"output": output, "parallel_tool_calls": true, "tools": req.Tools,
		}
	}

	if !req.Stream {
		var text string
		var output []any
		err := s.runTurn(r.Context(), tr, func(ev turnEvent) {
			switch ev.Kind {
			case "tool":
				output = append(output, map[string]any{
					"type": "function_call", "id": pick(ev.ID, newID("fc")),
					"call_id": pick(ev.ID, newID("call")), "name": ev.Name,
					"arguments": pick(ev.Args, "{}"), "status": "completed"})
			case "text":
				text += ev.Text
			case "error":
				text = "[" + ev.Text + "]"
			}
		})
		if err != nil {
			writeErr(w, 502, "upstream_error", err.Error())
			return
		}
		output = append(output, map[string]any{
			"type": "message", "id": newID("msg"), "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
		resp := base("completed", output)
		resp["output_text"] = text
		pt, ct := usageEstimate(prompt+system, text)
		resp["usage"] = map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": pt + ct}
		writeJSON(w, 200, resp)
		return
	}

	f := sseHeaders(w)
	ev := func(name string, payload map[string]any) {
		payload["type"] = name
		sseSend(w, f, name, payload)
	}
	outIdx := 0
	ev("response.created", map[string]any{"sequence_number": 0, "response": base("in_progress", []any{})})
	ev("response.in_progress", map[string]any{"sequence_number": 1, "response": base("in_progress", []any{})})

	var text string
	var fcItems []any // 收尾的 response.completed 要把 function_call item 一起带上
	seq := 2
	err = s.runTurn(r.Context(), tr, func(e turnEvent) {
		switch e.Kind {
		case "tool":
			itemID, callID := pick(e.ID, newID("fc")), pick(e.ID, newID("call"))
			fcItems = append(fcItems, map[string]any{"type": "function_call", "id": itemID,
				"call_id": callID, "name": e.Name, "arguments": pick(e.Args, "{}"), "status": "completed"})
			seq++
			ev("response.output_item.added", map[string]any{"sequence_number": seq, "output_index": outIdx,
				"item": map[string]any{"type": "function_call", "id": itemID, "call_id": callID,
					"name": e.Name, "arguments": ""}})
			seq++
			ev("response.function_call_arguments.delta", map[string]any{"sequence_number": seq,
				"item_id": itemID, "output_index": outIdx, "delta": pick(e.Args, "{}")})
			seq++
			ev("response.output_item.done", map[string]any{"sequence_number": seq, "output_index": outIdx,
				"item": map[string]any{"type": "function_call", "id": itemID, "call_id": callID,
					"name": e.Name, "arguments": pick(e.Args, "{}"), "status": "completed"}})
			outIdx++
		case "text":
			text += e.Text
		case "error":
			text = "[" + e.Text + "]"
		}
	})
	if err != nil {
		ev("error", map[string]any{"code": "upstream_error", "message": err.Error()})
		return
	}

	msgID := newID("msg")
	seq++
	ev("response.output_item.added", map[string]any{"sequence_number": seq, "output_index": outIdx,
		"item": map[string]any{"type": "message", "id": msgID, "role": "assistant",
			"status": "in_progress", "content": []any{}}})
	seq++
	ev("response.content_part.added", map[string]any{"sequence_number": seq, "item_id": msgID,
		"output_index": outIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	if text != "" {
		seq++
		ev("response.output_text.delta", map[string]any{"sequence_number": seq, "item_id": msgID,
			"output_index": outIdx, "content_index": 0, "delta": text})
	}
	seq++
	ev("response.output_text.done", map[string]any{"sequence_number": seq, "item_id": msgID,
		"output_index": outIdx, "content_index": 0, "text": text})
	seq++
	ev("response.content_part.done", map[string]any{"sequence_number": seq, "item_id": msgID,
		"output_index": outIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})
	finalItem := map[string]any{"type": "message", "id": msgID, "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	seq++
	ev("response.output_item.done", map[string]any{"sequence_number": seq, "output_index": outIdx,
		"item": finalItem})

	done := base("completed", append(append([]any{}, fcItems...), finalItem))
	done["output_text"] = text
	pt, ct := usageEstimate(prompt+system, text)
	done["usage"] = map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": pt + ct}
	seq++
	ev("response.completed", map[string]any{"sequence_number": seq, "response": done})
}
