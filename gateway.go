// gateway.go —— 把 prism 的 start/status 回合包装成 OpenAI / Anthropic 兼容接口。
//
// 翻译规则：调用方的 messages → prism 的
//   input[0] system : Prism 官方提示词 + 调用方 system + 工具清单 + 历史对话
//   input[1] user   : 最后一条用户消息
// 轮询期间沙箱产生的 toolCalls 会即时以各协议的工具调用事件流出，回合结束再补最终文本。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultModel = "gpt-6-astra"

var effortNames = []string{"minimal", "low", "medium", "high", "xhigh"}

// 由 main 从 prism_system_prompt.txt 载入（可选）
var prismSystemPrompt string

var (
	turnTimeout  = 8 * time.Minute
	pollInterval = 1200 * time.Millisecond
	modelFlag    string
	defaultEffort = "xhigh"
)

/* ---------------- 协议无关的回合模型 ---------------- */

type chatMsg struct {
	Role    string
	Content string
}

type toolDef struct {
	Type        string          `json:"type"`
	Function    *toolFunction   `json:"function,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"` // Anthropic 风格
	Parameters  json.RawMessage `json:"parameters,omitempty"`   // OpenAI / Codex Responses 风格
}

// displayName：取工具名（兼容平铺 name 与 function.name 两种写法）
func (t toolDef) displayName() string {
	if t.Function != nil && strings.TrimSpace(t.Function.Name) != "" {
		return strings.TrimSpace(t.Function.Name)
	}
	return strings.TrimSpace(t.Name)
}

// schema：取参数 schema（兼容 input_schema 与 parameters）
func (t toolDef) schema() json.RawMessage {
	if len(t.InputSchema) > 0 {
		return t.InputSchema
	}
	if t.Function != nil && len(t.Function.Parameters) > 0 {
		return t.Function.Parameters
	}
	return t.Parameters
}

/* ---------------- 工具清单缓存 ----------------
   Codex 桌面端不声明 tools（alpha 版走代码模式），但它运行时里存在 exec_command 等同名工具。
   这里缓存会声明工具的客户端（CLI）报上来的清单，遇到空清单时代它顶上。= */

const toolsCacheFile = "tools_cache.json"

var cachedTools struct {
	mu    sync.Mutex
	tools []toolDef
}

func cacheTools(tools []toolDef) {
	if len(tools) == 0 {
		return
	}
	cachedTools.mu.Lock()
	cachedTools.tools = tools
	cachedTools.mu.Unlock()
	if b, err := json.MarshalIndent(tools, "", "  "); err == nil {
		_ = os.WriteFile(toolsCacheFile, b, 0600)
	}
	log.Printf("工具清单已缓存: %v", declaredNames(tools))
}

func loadCachedTools() []toolDef {
	cachedTools.mu.Lock()
	defer cachedTools.mu.Unlock()
	if len(cachedTools.tools) == 0 {
		if txt, err := readTextFile(toolsCacheFile); err == nil && txt != "" {
			var ts []toolDef
			if json.Unmarshal([]byte(txt), &ts) == nil && len(ts) > 0 {
				cachedTools.tools = ts
				log.Printf("已从 %s 载入缓存的工具清单: %v", toolsCacheFile, declaredNames(ts))
			}
		}
	}
	return cachedTools.tools
}

// effectiveTools：客户端没声明工具时退回缓存（桌面端场景）
func effectiveTools(declared []toolDef) []toolDef {
	if len(declared) > 0 {
		cacheTools(declared)
		return declared
	}
	if cached := loadCachedTools(); len(cached) > 0 {
		log.Printf("桥接: 客户端未声明 tools，改用缓存的 %d 个: %v", len(cached), declaredNames(cached))
		return cached
	}
	return nil
}

// logRequestSummary：打印请求体的顶层字段与 tools 原文。
// 只记结构不记正文，避免把用户内容或凭据写进日志。
func logRequestSummary(path string, body []byte) {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		log.Printf("请求 %s：body %d 字节，不是 JSON 对象", path, len(body))
		return
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	log.Printf("请求 %s：%d 字节  顶层字段=%v", path, len(body), keys)
	for _, k := range keys {
		v := top[k]
		switch k {
		case "tools", "functions":
			log.Printf("    %s = %s", k, clip(string(v), 1500))
		case "input", "messages", "instructions", "system", "prompt", "metadata":
			log.Printf("    %s = (%d 字节，省略)", k, len(v))
		case "stream", "model", "tool_choice", "parallel_tool_calls", "store",
			"reasoning", "text", "include", "max_output_tokens", "prompt_cache_key", "conversation":
			log.Printf("    %s = %s", k, clip(string(v), 200))
		default:
			log.Printf("    %s = (%d 字节)", k, len(v))
		}
	}
	if _, has := top["tools"]; !has {
		log.Printf("    ⚠ 请求里没有 tools 字段")
	}
}

// logRawTools：把客户端原始 tools 打出来（对接真实客户端时用来核对 schema）
func logRawTools(tools []toolDef) {
	if !localTools {
		return
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return
	}
	log.Printf("桥接: 客户端原始 tools (%d 项): %s", len(tools), clip(string(raw), 800))
}

// declaredNames：列出调用方声明的工具名；空名会标出来（对接真实客户端时很常见）
func declaredNames(tools []toolDef) []string {
	out := make([]string, 0, len(tools))
	for i, t := range tools {
		n := t.displayName()
		if n == "" {
			n = fmt.Sprintf("(第%d个无名 type=%q)", i+1, t.Type)
		}
		out = append(out, n)
	}
	return out
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type turnRequest struct {
	Prompt         string
	System         string
	History        []chatMsg
	Model          string
	Effort         string
	Tools          []toolDef
	ConversationID string
}

// turnEvent 是回合运行器向协议适配层抛出的事件
type turnEvent struct {
	Kind string // tool | text | error
	ID   string
	Name string
	Args string
	Text string
}

/* ---------------- prism 上游响应结构 ---------------- */

type upstreamToolCall struct {
	LineIndex        int    `json:"line_index"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	CallType         string `json:"call_type"`
	ArgumentsPreview string `json:"arguments_preview"`
}

type upstreamResponse struct {
	Status  string `json:"status"`
	ID      string `json:"id"`
	Payload *struct {
		Reason   string `json:"reason"`
		Message  string `json:"message"`
		Output   []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		CodexDeltaFiles []struct {
			FilePath string `json:"file_path"`
			Status   string `json:"status"`
		} `json:"codexDeltaFiles"`
		// 沙箱侧失败的真实原因藏在 codexRequestDebug.error 里，
		// 例如 504 Timed out waiting for sandbox workspace file synchronization
		CodexRequestDebug *struct {
			SandboxTokenPresent   bool `json:"sandbox_token_present"`
			ListenSnapshotPresent bool `json:"listen_snapshot_present"`
			Error                 *struct {
				Status     int    `json:"status"`
				StatusText string `json:"statusText"`
				URL        string `json:"url"`
				BodyText   string `json:"bodyText"`
			} `json:"error"`
		} `json:"codexRequestDebug"`
	} `json:"payload"`
}

// failure：把上游的失败原因整理成一句话；有沙箱侧细节时优先用它
func (r *upstreamResponse) failure() string {
	if r == nil || r.Payload == nil {
		return ""
	}
	if d := r.Payload.CodexRequestDebug; d != nil && d.Error != nil {
		e := d.Error
		detail := strings.TrimSpace(e.BodyText)
		var b struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(detail), &b) == nil && b.Error.Message != "" {
			detail = b.Error.Message
		}
		return fmt.Sprintf("沙箱侧 %d %s：%s", e.Status, e.StatusText, clip(detail, 200))
	}
	return r.Payload.Reason
}

type upstreamState struct {
	Status         string            `json:"status"`
	RequestID      string            `json:"request_id"`
	ConversationID string            `json:"conversation_id"`
	TurnState      json.RawMessage   `json:"turn_state"`
	Response       *upstreamResponse `json:"response"`
	CodexLive      *struct {
		LineCount int                `json:"lineCount"`
		ToolCalls []upstreamToolCall `json:"toolCalls"`
	} `json:"codex_live_progress"`
}

func (r *upstreamResponse) text() string {
	if r == nil || r.Payload == nil {
		return ""
	}
	var b strings.Builder
	for _, o := range r.Payload.Output {
		if o.Type != "message" || o.Role != "assistant" {
			continue
		}
		for _, c := range o.Content {
			if c.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(c.Text)
			}
		}
	}
	return b.String()
}

/* ---------------- 提示词拼装 ---------------- */

func (tr turnRequest) systemText() string {
	var parts []string
	if p := strings.TrimSpace(prismSystemPrompt); p != "" {
		parts = append(parts, p)
	}
	if s := strings.TrimSpace(tr.System); s != "" {
		parts = append(parts, "## 调用方系统指令\n"+s)
	}
	if len(tr.Tools) > 0 {
		parts = append(parts, renderTools(tr.Tools))
	}
	if len(tr.History) > 0 {
		parts = append(parts, "## 本次请求之前的对话\n"+renderHistory(tr.History))
	}
	return strings.Join(parts, "\n\n")
}

func renderTools(tools []toolDef) string {
	var b strings.Builder
	b.WriteString("## 调用方声明的工具\n以下工具由调用方定义（沙箱内不直接执行，仅供你理解可用能力）：\n")
	for _, t := range tools {
		name, desc, schema := t.Name, t.Description, t.InputSchema
		if t.Function != nil {
			name, desc, schema = t.Function.Name, t.Function.Description, t.Function.Parameters
		}
		fmt.Fprintf(&b, "- %s", name)
		if desc != "" {
			fmt.Fprintf(&b, "：%s", desc)
		}
		if len(schema) > 0 {
			fmt.Fprintf(&b, "\n  参数 schema: %s", string(schema))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderHistory(hist []chatMsg) string {
	var b strings.Builder
	for _, m := range hist {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, strings.TrimSpace(m.Content))
	}
	return b.String()
}

// splitConversation：system 合并成一段，最后一条非 system 消息作为本轮 prompt，其余进历史。
// 全是 system 的消息（有些客户端会发这种探活请求）不再报错，退回用最后一条当 prompt。
func splitConversation(msgs []chatMsg) (system, prompt string, hist []chatMsg, err error) {
	last := -1
	for i, m := range msgs {
		if m.Role != "system" && strings.TrimSpace(m.Content) != "" {
			last = i
		}
	}
	if last >= 0 {
		prompt = msgs[last].Content
		hist = append(append([]chatMsg{}, msgs[:last]...), msgs[last+1:]...)
	} else if n := len(msgs); n > 0 {
		prompt = msgs[n-1].Content
		hist = append([]chatMsg{}, msgs[:n-1]...)
	}
	var sys []string
	for _, m := range hist {
		if m.Role == "system" && strings.TrimSpace(m.Content) != "" {
			sys = append(sys, strings.TrimSpace(m.Content))
		}
	}
	system = strings.Join(sys, "\n\n")
	if strings.TrimSpace(prompt) == "" {
		prompt, system = system, ""
	}
	if strings.TrimSpace(prompt) == "" {
		err = fmt.Errorf("messages 为空")
	}
	return
}

/* ---------------- 回合运行器 ---------------- */

func (s *session) runTurn(ctx context.Context, tr turnRequest, emit func(turnEvent)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cookie := s.cookieOr("")
	if cookie == "" {
		return fmt.Errorf("服务端没有可用 cookie")
	}
	if strings.TrimSpace(tr.Prompt) == "" {
		return fmt.Errorf("prompt 为空")
	}

	effort := pick(tr.Effort, defaultEffort)
	if localTools {
		// 桥接模式只要求模型输出一小段 JSON，用最低推理强度，省钱省时间
		effort = "low"
	}
	meta := map[string]any{
		"projectId":        bootstrap.ProjectID,
		"userId":           bootstrap.UserID,
		"model":            pick(tr.Model, defaultModel),
		"reasoning_effort": effort,
		"frontend_origin":  baseURL,
		"sandbox_url":      pick(bootstrap.SandboxURL, baseURL+"/s/sandboxes/proxy/"),
	}
	if bootstrap.SandboxToken != "" {
		meta["sandbox_token"] = bootstrap.SandboxToken
	}
	if bootstrap.ListenSnapshot != "" {
		meta["codex_listen_snapshot"] = json.RawMessage(bootstrap.ListenSnapshot)
	}

	// 桥接模式用代理协议替换默认系统文本：不能同时塞 Prism 人格提示词，
	// 否则「用你自己的工作」和「只用 JSON 下单」两条指令会互相打架。
	sysText := tr.systemText()
	if localTools {
		if len(tr.Tools) == 0 {
			log.Printf("桥接: 请求没有声明 tools，尝试用缓存清单顶上")
		} else {
			log.Printf("桥接: 请求声明了 %d 个工具: %v", len(tr.Tools), declaredNames(tr.Tools))
		}
		// 桌面端不声明工具，用缓存（CLI 报过的清单）代替
		tr.Tools = effectiveTools(tr.Tools)
	}
	switch {
	case localTools && len(tr.Tools) > 0:
		sysText = tr.bridgeSystemText()
	case localTools:
		// 完全没有可用清单：必须明确告诉模型"你碰不到用户的电脑"，
		// 否则它会在自己的沙箱里建文件、然后回复"已创建"，让用户以为本机已经有了。
		sysText = sysText + "\n\n" + noLocalToolsNotice
	}
	input := []any{}
	if sysText != "" {
		input = append(input, map[string]any{"type": "message", "role": "system",
			"content": []any{map[string]any{"type": "input_text", "text": sysText}}})
	}
	input = append(input, map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": tr.Prompt}}})

	body := map[string]any{"input": input, "metadata": meta}
	if conv := pick(tr.ConversationID, bootstrap.ConversationID); conv != "" {
		body["conversationId"] = conv
	}

	// start 会创建回合，不幂等：只在传输层失败（请求多半没到）时重试一次
	code, raw, err := upstreamRetry(ctx, startPath, body, cookie, nil, 2)
	if err != nil {
		return err
	}
	var st upstreamState
	_ = json.Unmarshal([]byte(raw), &st)
	if code != 200 {
		return fmt.Errorf("上游 start %d: %s", code, clip(raw, 300))
	}
	s.remember(raw)
	if st.TurnState == nil {
		if msg := st.Response.failure(); msg != "" {
			if st.Response != nil && st.Response.Payload != nil &&
				st.Response.Payload.CodexRequestDebug != nil &&
				st.Response.Payload.CodexRequestDebug.Error != nil {
				msg += "（沙箱工作区恢复失败：回浏览器重新打开该项目，再跑 grab_token.py 更新 bootstrap.json）"
			} else {
				msg += "（沙箱未就绪或凭据缺失，可先打 heartbeat 预热）"
			}
			emit(turnEvent{Kind: "error", Text: msg})
			return nil
		}
		return fmt.Errorf("start 未返回 turn_state（HTTP %d）", code)
	}
	if st.Status == "completed" {
		emitAnswer(tr, st.Response, emit)
		return nil
	}

	rid, ts := st.RequestID, st.TurnState
	seen := map[string]bool{}
	deadline := time.Now().Add(turnTimeout)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		if time.Now().After(deadline) {
			emit(turnEvent{Kind: "error", Text: "回合超时未完成"})
			return nil
		}
		// status 是只读轮询，多试几次
		pcode, praw, perr := upstreamRetry(ctx, statusPath,
			map[string]any{"request_id": rid, "turn_state": ts}, cookie, nil, 3)
		if perr != nil {
			return perr
		}
		if pcode != 200 {
			return fmt.Errorf("上游 status %d: %s", pcode, clip(praw, 300))
		}
		var ps upstreamState
		_ = json.Unmarshal([]byte(praw), &ps)
		if len(ps.TurnState) > 0 {
			ts = ps.TurnState // 原样回传，游标会推进
		}
		// 沙箱工具调用：即时流出（只在 pending 期间出现）
		if ps.CodexLive != nil {
			for _, tc := range ps.CodexLive.ToolCalls {
				key := tc.CallID
				if key == "" {
					key = fmt.Sprint("line:", tc.LineIndex)
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				emit(turnEvent{Kind: "tool", ID: key, Name: pick(tc.Name, "tool"), Args: tc.ArgumentsPreview})
			}
		}
		switch ps.Status {
		case "completed":
			emitAnswer(tr, ps.Response, emit)
			return nil
		case "failed", "error":
			msg := "回合失败"
			if ps.Response != nil && ps.Response.Payload != nil && ps.Response.Payload.Reason != "" {
				msg = ps.Response.Payload.Reason
			}
			emit(turnEvent{Kind: "error", Text: msg})
			return nil
		}
	}
}

/* ---------------- 客户端工具桥接（-local-tools） ---------------- */

// 上游没有 function calling 通道：客户端声明的 tools 拿不到模型的原生 tool_call。
// 桥接模式下改成「模型用 JSON 下单 → 网关翻译成标准 tool_calls → 本机 agent 执行 →
// 结果以 tool_result / role=tool 回灌下一轮」。
var localTools bool

// noLocalToolsNotice：客户端没有声明任何本机工具时用的环境说明。
// 不加这段，模型会在自己的沙箱里把事情做完，然后告诉用户"已创建"——用户以为本机有了，其实没有。
const noLocalToolsNotice = `## 环境说明（重要）
本次请求的客户端**没有声明任何本机工具**，因此你**无法访问用户的电脑**。
- 你只能在自己的沙箱工作区里读写文件；那与用户的电脑无关。
- **绝对不要**声称"已在你的电脑/桌面/磁盘上创建或修改了文件"。在沙箱里建了文件也**不算**完成用户的请求。
- 如果用户要求操作他本机的文件，请如实说明：当前客户端没有向模型声明本机工具，所以你做不到；
  并建议他改用会执行本机工具的客户端（例如仓库里的 local_agent.py），或让网关开启本机执行模式。`

const toolProxyProtocol = `## 客户端工具代理模式（必须遵守）
你的沙箱工具不适用于本次任务。需要执行动作时，你只能输出一个 JSON 对象来表达要调用的客户端工具，
不要解释、不要用命令行、不要读写沙箱文件：

{"tool_calls":[{"name":"<工具名>","arguments":{...}}]}

规则：
- 一次可以给多个条目，调用方会按顺序执行。
- 调用方会把执行结果以 [tool_result ...]（或 role=tool 的消息）放进后续对话里，你看到结果后继续。
- 本轮如果不需要工具就能回答，就直接用纯文本回答，不要输出 JSON。
- 工具名必须来自下面列出的清单，参数必须符合其 schema。

## 在 Windows 上执行命令的注意事项（能省好几轮）
- 调用方多半是 Codex，exec_command 在 Windows 上跑的是 **PowerShell**，不是 bash：
  不要用 ls / cat / [ -f x ] / && 这类写法，改用 Get-ChildItem / Get-Content / Test-Path。
- **不要把补丁文本或 heredoc 嵌进 PowerShell 命令**：apply_patch 会因引号转义失败，
  报 The first line of the patch must be *** Begin Patch。写文件请直接：
  Set-Content -LiteralPath '文件' -Value '内容' -Encoding UTF8
- 内容含引号时用 -Value 传单个字符串，或分多次 Add-Content -Encoding UTF8 追加；
  路径一律用 -LiteralPath。`

func renderToolsForProxy(tools []toolDef) string {
	var b strings.Builder
	b.WriteString("\n\n可用工具清单（**必须**从中选名字，不要自己发明）：\n")
	for _, t := range tools {
		desc := t.Description
		if t.Function != nil && t.Function.Description != "" {
			desc = t.Function.Description
		}
		fmt.Fprintf(&b, "- %s", t.displayName())
		if desc != "" {
			fmt.Fprintf(&b, "：%s", clip(desc, 200))
		}
		b.WriteString("\n")
		if s := t.schema(); len(s) > 0 {
			fmt.Fprintf(&b, "  参数 schema: %s\n", clip(string(s), 600))
		}
	}
	return b.String()
}

type parsedCall struct{ Name, Args string }

var (
	fenceRe     = regexp.MustCompile("(?s)```[a-zA-Z_]*\\s*(\\{.*?\\})\\s*```")
	commaTailRe = regexp.MustCompile(`,\s*([}\]])`)
	nameStrip   = strings.NewReplacer("-", "", "_", "", " ", "", "\t", "")
)

// normName：工具名归一化，容忍大小写/下划线/连字符差异
func normName(s string) string {
	return strings.ToLower(nameStrip.Replace(strings.TrimSpace(s)))
}

// jsonCandidates：扫出文本里所有「花括号平衡」的片段（跳过字符串内的括号）
func jsonCandidates(s string) []string {
	var out []string
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case !inStr && c == '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, s[start:i+1])
				}
			}
		}
	}
	return out
}

// parseToolCalls：从模型输出里抽工具调用。
// 容忍：代码块、前后夹带说明文字、嵌套花括号、markdown 强调、中文弯引号、尾逗号、
// arguments 双重编码、多种键名（name/tool/tool_name/function/args/input/parameters）。
// 关键：**不再因为工具名不在声明列表里就把 JSON 漏成纯文本**——那样客户端只会收到一段 JSON 文本。
func parseToolCalls(text string, tools []toolDef) []parsedCall {
	allowed := map[string]string{}
	for _, t := range tools {
		n := t.Name
		if t.Function != nil {
			n = t.Function.Name
		}
		if strings.TrimSpace(n) != "" {
			allowed[normName(n)] = n
		}
	}
	cleaned := strings.NewReplacer("“", "\"", "”", "\"", "‘", "'", "’", "'",
		"**", "", "`", "").Replace(text)

	var cands []string
	for _, m := range fenceRe.FindAllStringSubmatch(text, -1) {
		cands = append(cands, m[1])
	}
	cands = append(cands, jsonCandidates(cleaned)...)
	cands = append(cands, jsonCandidates(text)...)

	for _, c := range cands {
		if calls := decodeToolCalls(c, allowed); len(calls) > 0 {
			return calls
		}
	}
	if strings.Contains(text, "tool_calls") {
		log.Printf("桥接: 输出里出现 tool_calls 但解析失败（请把这段发给作者）: %s", clip(text, 300))
	}
	return nil
}

// rawCall：兼容各家把工具调用写成不同键名的习惯
type rawCall struct {
	Name       string          `json:"name"`
	ToolName   string          `json:"tool_name"`
	Tool       string          `json:"tool"`
	Function   json.RawMessage `json:"function"`
	Arguments  json.RawMessage `json:"arguments"`
	Args       json.RawMessage `json:"args"`
	Input      json.RawMessage `json:"input"`
	Parameters json.RawMessage `json:"parameters"`
}

func (c rawCall) name() string { return pick(c.Name, c.ToolName, c.Tool) }

func (c rawCall) args() json.RawMessage {
	for _, r := range []json.RawMessage{c.Arguments, c.Args, c.Input, c.Parameters} {
		if len(r) > 0 {
			return r
		}
	}
	return nil
}

func decodeToolCalls(s string, allowed map[string]string) []parsedCall {
	s = commaTailRe.ReplaceAllString(strings.TrimSpace(s), "$1")

	var probe struct {
		ToolCalls []rawCall `json:"tool_calls"`
		Calls     []rawCall `json:"calls"`
		Action    *rawCall  `json:"action"`
		rawCall
	}
	if json.Unmarshal([]byte(s), &probe) != nil {
		return nil
	}

	var out []parsedCall
	add := func(name string, raw json.RawMessage) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if real, ok := allowed[normName(name)]; ok {
			name = real
		} else if len(allowed) > 0 {
			log.Printf("桥接: 模型用了未声明的工具名 %q，仍按工具调用返回（声明：%v）", name, toolNames(allowed))
		}
		out = append(out, parsedCall{Name: name, Args: normalizeArgs(raw)})
	}
	collect := func(c rawCall) {
		n, raw := c.name(), c.args()
		if len(c.Function) > 0 {
			var fn rawCall
			if json.Unmarshal(c.Function, &fn) == nil && (fn.name() != "" || len(fn.args()) > 0) {
				if n == "" {
					n = fn.name()
				}
				if len(raw) == 0 {
					raw = fn.args()
				}
			} else {
				var fname string
				if json.Unmarshal(c.Function, &fname) == nil && n == "" {
					n = fname
				}
			}
		}
		add(n, raw)
	}

	// 数组形式（tool_calls / calls）：是明确的调用结构，一律接受
	for _, tc := range probe.ToolCalls {
		collect(tc)
	}
	if len(out) == 0 {
		for _, tc := range probe.Calls {
			collect(tc)
		}
	}
	// 单对象形式：必须像一次调用才接受，避免把普通 JSON 误判
	if len(out) == 0 && probe.Action != nil {
		collect(*probe.Action)
	}
	if len(out) == 0 {
		single := probe.rawCall
		callLike := single.Tool != "" || single.ToolName != "" || len(single.Function) > 0 ||
			(single.Name != "" && len(single.args()) > 0)
		if callLike {
			collect(single)
		}
	}
	return out
}

func toolNames(allowed map[string]string) []string {
	out := make([]string, 0, len(allowed))
	for _, v := range allowed {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// normalizeArgs：兼容 arguments 被写成 JSON 字符串（双重编码）的情况
func normalizeArgs(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "{}"
	}
	if strings.HasPrefix(s, "\"") {
		var inner string
		if json.Unmarshal(raw, &inner) == nil && strings.TrimSpace(inner) != "" {
			return strings.TrimSpace(inner)
		}
	}
	return s
}

func (tr turnRequest) bridgeSystemText() string {
	var parts []string
	if s := strings.TrimSpace(tr.System); s != "" {
		parts = append(parts, "## 调用方系统指令\n"+s)
	}
	parts = append(parts, toolProxyProtocol+renderToolsForProxy(tr.Tools))
	if len(tr.History) > 0 {
		parts = append(parts, "## 本次请求之前的对话（含工具执行结果）\n"+renderHistory(tr.History))
	}
	return strings.Join(parts, "\n\n")
}

// emitAnswer：桥接模式下把模型输出的 JSON 翻译成工具调用，否则按普通文本返回
func emitAnswer(tr turnRequest, resp *upstreamResponse, emit func(turnEvent)) {
	text := resp.text()
	// 注意：这里不要求 len(tr.Tools) > 0。客户端没把 tools 传进来（或形状没解析出来）时，
	// 模型仍可能按之前的协议输出 JSON —— 那种情况下也必须转成工具调用，不能漏成纯文本。
	if localTools {
		log.Printf("桥接: 模型原始输出: %s", clip(text, 300))
		if calls := parseToolCalls(text, tr.Tools); len(calls) > 0 {
			names := make([]string, 0, len(calls))
			for _, c := range calls {
				names = append(names, c.Name)
			}
			log.Printf("桥接: 识别到 %d 个工具调用 (%s)", len(calls), strings.Join(names, ", "))
			for _, c := range calls {
				emit(turnEvent{Kind: "tool", ID: newID("call"), Name: c.Name, Args: c.Args})
			}
			return
		}
	}
	emit(turnEvent{Kind: "text", Text: text})
}

/* ---------------- 保存 bootstrap（UI 的「保存」按钮） ---------------- */

// 必须落盘：UI 里填的只影响页面自己发的请求，/v1 调用仍会回退到启动时载入的 bootstrap。
var bootstrapPath, cookieFilePath string

type bootstrapForm struct {
	Cookie         string `json:"cookie"`
	SandboxToken   string `json:"sandbox_token"`
	SandboxURL     string `json:"sandbox_url"`
	ProjectID      string `json:"project_id"`
	UserID         string `json:"user_id"`
	ConversationID string `json:"conversation_id"`
	ListenSnapshot string `json:"listen_snapshot"`
}

func (s *session) handleSaveBootstrap(w http.ResponseWriter, r *http.Request) {
	var f bootstrapForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 只覆盖填了值的字段，避免空表单把已有配置清掉
	set := func(dst *string, v string) {
		if strings.TrimSpace(v) != "" {
			*dst = strings.TrimSpace(v)
		}
	}
	set(&bootstrap.SandboxToken, f.SandboxToken)
	set(&bootstrap.SandboxURL, f.SandboxURL)
	set(&bootstrap.ProjectID, f.ProjectID)
	set(&bootstrap.UserID, f.UserID)
	set(&bootstrap.ConversationID, f.ConversationID)
	set(&bootstrap.ListenSnapshot, f.ListenSnapshot)

	s.mu.Lock()
	if c := strings.TrimSpace(f.Cookie); c != "" {
		s.cookie = c
	}
	cookie := s.cookie
	s.mu.Unlock()
	bootstrap.Cookie = cookie

	saved := ""
	if bootstrapPath != "" {
		b, err := json.MarshalIndent(bootstrap, "", "  ")
		if err != nil {
			writeErr(w, 500, "encode_failed", err.Error())
			return
		}
		if err := os.WriteFile(bootstrapPath, b, 0600); err != nil {
			writeErr(w, 500, "write_failed", err.Error())
			return
		}
		saved = bootstrapPath
	}
	if cookieFilePath != "" && cookie != "" {
		_ = os.WriteFile(cookieFilePath, []byte(cookie), 0600)
	}
	log.Printf("bootstrap 已保存到 %s（token %d 字节, snapshot %d 字节, conv=%s）",
		saved, len(bootstrap.SandboxToken), len(bootstrap.ListenSnapshot), bootstrap.ConversationID)
	writeJSON(w, 200, map[string]any{
		"ok": true, "saved_to": saved,
		"sandbox_token_len":   len(bootstrap.SandboxToken),
		"listen_snapshot_len": len(bootstrap.ListenSnapshot),
		"conversation_id":     bootstrap.ConversationID,
		"cookie_len":          len(cookie),
	})
}

/* ---------------- 模型 ---------------- */

func modelsList() []string {
	if strings.TrimSpace(modelFlag) != "" {
		var out []string
		for _, m := range strings.Split(modelFlag, ",") {
			if m = strings.TrimSpace(m); m != "" {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	list := []string{defaultModel}
	for _, e := range effortNames {
		list = append(list, defaultModel+"-"+e)
	}
	return list
}

// parseModel：支持 "gpt-6-astra-high" 这种把 reasoning_effort 编码进模型名的写法
func parseModel(m string) (string, string) {
	m = strings.TrimSpace(m)
	if m == "" {
		return defaultModel, defaultEffort
	}
	low := strings.ToLower(m)
	for _, e := range effortNames {
		if strings.HasSuffix(low, "-"+e) {
			return m[:len(m)-len(e)-1], e
		}
	}
	return m, defaultEffort
}

func (s *session) handleModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	one := strings.TrimPrefix(r.URL.Path, "/v1/models")
	if one != "" && one != "/" {
		id := strings.TrimPrefix(one, "/")
		for _, m := range modelsList() {
			if m == id {
				writeJSON(w, 200, map[string]any{"id": id, "object": "model",
					"created": now, "owned_by": "prism"})
				return
			}
		}
		writeErr(w, 404, "model_not_found", "未知模型 "+id)
		return
	}
	data := []map[string]any{}
	for _, m := range modelsList() {
		data = append(data, map[string]any{"id": m, "object": "model",
			"created": now, "owned_by": "prism"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

/* ---------------- 通用小工具 ---------------- */

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "code": typ, "param": nil}})
}

func sseHeaders(w http.ResponseWriter) http.Flusher {
	h := w.Header()
	h.Set("content-type", "text/event-stream; charset=utf-8")
	h.Set("cache-control", "no-cache")
	h.Set("connection", "keep-alive")
	h.Set("x-accel-buffering", "no")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	if f != nil {
		f.Flush()
	}
	return f
}

func sseSend(w io.Writer, f http.Flusher, event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("sse marshal: %v", err)
		return
	}
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f != nil {
		f.Flush()
	}
}

func sseRaw(w io.Writer, f http.Flusher, s string) {
	fmt.Fprint(w, s)
	if f != nil {
		f.Flush()
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func newID(prefix string) string {
	return fmt.Sprintf("%s_%d%04d", prefix, time.Now().Unix(), time.Now().Nanosecond()/100000)
}

// usageEstimate：上游不回传 token 用量，这里按字符数估算（客户端多数只用于记账）
func usageEstimate(prompt, completion string) (int, int) {
	return len(prompt)/4 + 1, len(completion)/4 + 1
}
