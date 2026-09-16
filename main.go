// prismctl — 用浏览器 cookie + sandbox bootstrap 驱动 prism.openai.com 的对话接口。
//
// 实证通过的链路（2026-09-16）：
//   GET  /s/sandboxes/proxy/heartbeat            带 x-crixet-sandbox-token，预热沙箱
//   POST /api/llm/response_with_tools_start      metadata 里必须带 sandbox_token
//                                                + codex_listen_snapshot，否则 200 但
//                                                reason=sandbox_reconnecting（没法对话）
//   POST /api/llm/response_with_tools_status     必须把 start 回传的 turn_state 原样回传，
//                                                否则 401；completed 时答案在
//                                                response.payload.output[].content[].text
//
// 认证：Cookie（必须含 oai-sc / prism_session_token / prism_oai_access_token / cf_clearance）。
// cf_clearance 与出口 IP + UA 绑定，故 UA 与浏览器一致。
//
// 启动：go run main.go -addr 127.0.0.1:8899 [-bootstrap bootstrap.json]
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	baseURL    = "https://prism.openai.com"
	startPath  = "/api/llm/response_with_tools_start"
	statusPath = "/api/llm/response_with_tools_status"
	hbPath     = "/s/sandboxes/proxy/heartbeat"
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
)

// TLSClientConfig 非 nil 且不强制 h2 → 走 HTTP/1.1。
// Cloudflare 上 Go 的 http2 连接复用会偶发 "connection error: PROTOCOL_ERROR"。
var httpc = &http.Client{Transport: &http.Transport{
	ResponseHeaderTimeout: 300 * time.Second,
	IdleConnTimeout:       60 * time.Second,
	TLSClientConfig:       &tls.Config{},
}}

// upstreamRetry：传输层错误（连接被 RST/GOAWAY、EOF）重试；apply 型请求由调用方控制次数
func upstreamRetry(ctx context.Context, path string, body any, cookie string,
	extra map[string]string, attempts int) (int, string, error) {
	var code int
	var raw string
	var err error
	for i := 0; i < attempts; i++ {
		code, raw, err = upstreamCtx(ctx, path, body, cookie, extra)
		if err == nil {
			return code, raw, nil
		}
		if ctx.Err() != nil {
			return code, raw, err
		}
		log.Printf("%s 第 %d 次传输失败，重试: %v", path, i+1, err)
		time.Sleep(time.Duration(250*(i+1)) * time.Millisecond)
	}
	return code, raw, err
}

// minimalContext：system 消息里那段"上下文 JSON"，缺省用一个空工程占位
const minimalContext = `{"openFile":{"status":"error","message":"No open file"},"selectedText":{"status":"error","message":"No selection"},"request":{"source":"user","promptTextLength":0,"timestampUtcIso":"","timestampUtcMs":0,"selectionKind":"unknown"}}`

type session struct {
	mu        sync.Mutex
	cookie    string
	requestID string
	convID    string
	turnState json.RawMessage
	lastDone  bool
	hasTurn   bool
}

// bootstrap.json：由 grab_token.py 从 HAR 里抠出来
var bootstrap struct {
	Cookie         string `json:"cookie"`
	SandboxToken   string `json:"sandbox_token"`
	SandboxURL     string `json:"sandbox_url"`
	ConversationID string `json:"conversation_id"`
	ProjectID      string `json:"project_id"`
	UserID         string `json:"user_id"`
	ListenSnapshot string `json:"listen_snapshot"`
}

func upstream(path string, body any, cookie string, extra map[string]string) (int, string, error) {
	return upstreamCtx(context.Background(), path, body, cookie, extra)
}

func upstreamCtx(ctx context.Context, path string, body any, cookie string, extra map[string]string) (int, string, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, rdr)
	if err != nil {
		return 0, "", err
	}
	if body == nil {
		req.Method = http.MethodGet
	}
	h := req.Header
	h.Set("accept", "*/*")
	h.Set("accept-language", "zh-CN,zh;q=0.9")
	h.Set("origin", baseURL)
	h.Set("referer", baseURL+"/")
	h.Set("priority", "u=1, i")
	h.Set("sec-ch-ua", `"Not=A?Brand";v="99", "Google Chrome";v="151", "Chromium";v="151"`)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
	h.Set("user-agent", userAgent)
	if body != nil {
		h.Set("content-type", "application/json")
	}
	if cookie != "" {
		h.Set("cookie", cookie)
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// readTextFile：读文本并去掉 UTF-8 BOM。
// Windows 上 Set-Content -Encoding UTF8、记事本写出的文件带 BOM，
// 会让 json.Unmarshal 直接报错（BOM 不是合法 JSON 开头）。
func readTextFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	return strings.TrimSpace(string(b)), nil
}

// remember：记下 request_id / turn_state / conversation_id，供 status 复用
func (s *session) remember(respBody string) {
	var m struct {
		Status         string          `json:"status"`
		RequestID      string          `json:"request_id"`
		ConversationID string          `json:"conversation_id"`
		TurnState      json.RawMessage `json:"turn_state"`
	}
	if json.Unmarshal([]byte(respBody), &m) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.RequestID != "" {
		s.requestID = m.RequestID
	}
	if m.ConversationID != "" {
		s.convID = m.ConversationID
	}
	if len(m.TurnState) > 0 {
		s.turnState = m.TurnState
		s.hasTurn = true
	}
	s.lastDone = m.Status == "completed"
}

func (s *session) cookieOr(c string) string {
	c = strings.TrimSpace(c)
	if c != "" {
		s.mu.Lock()
		s.cookie = c
		s.mu.Unlock()
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cookie
}

// pick 返回第一个非空值；表单 > bootstrap > 内置缺省。
func pick(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

type startForm struct {
	Cookie, Prompt, Context, ConversationID, PreviousResponseID string
	SandboxToken, ListenSnapshot, ProxyDebug, SandboxURL         string
	Model, ReasoningEffort, ProjectID, UserID                    string
}

func (s *session) handleStart(w http.ResponseWriter, r *http.Request) {
	var f startForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cookie := s.cookieOr(f.Cookie)
	if cookie == "" {
		http.Error(w, "cookie 为空", http.StatusBadRequest)
		return
	}
	ctx := f.Context
	if strings.TrimSpace(ctx) == "" {
		ctx = minimalContext
	}
	sandboxToken := pick(f.SandboxToken, bootstrap.SandboxToken)
	sandboxURL := pick(f.SandboxURL, bootstrap.SandboxURL, baseURL+"/s/sandboxes/proxy/")
	meta := map[string]any{
		"projectId":        pick(f.ProjectID, bootstrap.ProjectID),
		"userId":           pick(f.UserID, bootstrap.UserID),
		"model":            pick(f.Model, "gpt-6-astra"),
		"reasoning_effort": pick(f.ReasoningEffort, "xhigh"),
		"frontend_origin":  baseURL,
		"sandbox_url":      sandboxURL,
	}
	// 这两个是能否真正对话的关键：缺 sandbox_token 就只会得到 sandbox_reconnecting
	if sandboxToken != "" {
		meta["sandbox_token"] = sandboxToken
	}
	if snap := pick(f.ListenSnapshot, bootstrap.ListenSnapshot); snap != "" {
		meta["codex_listen_snapshot"] = json.RawMessage(snap)
	}
	if f.ProxyDebug != "" {
		meta["proxy_request_debug"] = json.RawMessage(f.ProxyDebug)
	}
	body := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "system", "content": []any{
				map[string]any{"type": "input_text", "text": ctx}}},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": f.Prompt}}},
		},
		"metadata": meta,
	}
	// listen_snapshot / sandbox_token 都是绑定到某个会话的，conversationId 必须配套；
	// 缺省用 bootstrap 里那一个，否则上游会当新会话处理并报 sandbox_reconnecting。
	conv := f.ConversationID
	if conv == "" {
		conv = bootstrap.ConversationID
	}
	if conv != "" {
		body["conversationId"] = conv
	}
	if f.PreviousResponseID != "" {
		body["previousResponseId"] = f.PreviousResponseID
	}
	code, respBody, err := upstreamRetry(r.Context(), startPath, body, cookie, nil, 2)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("start -> %d (%d bytes)", code, len(respBody))
	s.remember(respBody)
	w.Header().Set("x-upstream-status", fmt.Sprint(code))
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, respBody)
}

type statusForm struct {
	Cookie       string `json:"cookie"`
	RequestID    string `json:"requestId"`
	TurnStateRaw string `json:"turnStateRaw"`
}

func (s *session) handleStatus(w http.ResponseWriter, r *http.Request) {
	var f statusForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cookie := s.cookieOr(f.Cookie)
	s.mu.Lock()
	rid, ts := s.requestID, s.turnState
	s.mu.Unlock()
	if strings.TrimSpace(f.RequestID) != "" {
		rid = strings.TrimSpace(f.RequestID)
	}
	if strings.TrimSpace(f.TurnStateRaw) != "" {
		ts = json.RawMessage(f.TurnStateRaw)
	}
	if rid == "" {
		http.Error(w, "没有 request_id：先跑一次 start", http.StatusBadRequest)
		return
	}
	if len(ts) == 0 {
		http.Error(w, "没有 turn_state：必须原样复用 start 回传的那份，否则上游 401", http.StatusBadRequest)
		return
	}
	code, respBody, err := upstreamRetry(r.Context(), statusPath,
		map[string]any{"request_id": rid, "turn_state": ts}, cookie, nil, 3)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("status -> %d (%d bytes)", code, len(respBody))
	s.remember(respBody)
	w.Header().Set("x-upstream-status", fmt.Sprint(code))
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, respBody)
}

// handleHeartbeat：探活/预热沙箱（前端 worker 也在打这个）
func (s *session) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var f struct {
		Cookie       string `json:"cookie"`
		SandboxToken string `json:"sandboxToken"`
	}
	json.NewDecoder(r.Body).Decode(&f)
	cookie := s.cookieOr(f.Cookie)
	// 没传就退回 bootstrap 里的 token，否则代理层会因为缺少 token 直接 404
	token := pick(f.SandboxToken, bootstrap.SandboxToken)
	code, respBody, err := upstreamRetry(r.Context(), hbPath+"?prism_cache_bust="+fmt.Sprint(time.Now().UnixMilli()),
		nil, cookie, map[string]string{"x-crixet-sandbox-token": token}, 3)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("heartbeat -> %d", code)
	w.Header().Set("x-upstream-status", fmt.Sprint(code))
	w.WriteHeader(code)
	io.WriteString(w, respBody)
}

func (s *session) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"request_id":      s.requestID,
		"conversation_id": s.convID,
		"has_turn_state":  s.hasTurn,
		"last_completed":  s.lastDone,
		"cookie_loaded":   len(s.cookie) > 0,
		"bootstrap":       bootstrap,
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(out)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8899", "监听地址")
	html := flag.String("html", "index.html", "网页 UI 文件")
	bsFile := flag.String("bootstrap", "", "由 grab_token.py 生成的 bootstrap.json")
	cookieFile := flag.String("cookie-file", "", "可选：cookie 落盘路径（明文，注意别入库）")
	promptFile := flag.String("prism-prompt", "", "注入 Prism 官方系统提示词（默认关闭；填 prism_system_prompt.txt 可让模型扮演网页端那个 LaTeX 助手）")
	models := flag.String("models", "", "逗号分隔的模型白名单，默认 gpt-6-astra 及其 effort 变体")
	effort := flag.String("effort", "xhigh", "默认 reasoning_effort")
	timeout := flag.Duration("turn-timeout", 8*time.Minute, "单轮最长等待时间")
	lt := flag.Bool("local-tools", false, "桥接模式：模型用 JSON 下单，把客户端声明的 tools 翻成真正的 tool_calls 交给本机执行")
	flag.Parse()

	localTools = *lt
	if localTools {
		log.Printf("已启用客户端工具桥接（-local-tools）：模型不再自己动手，改为输出 JSON 由调用方执行")
	}
	if *models != "" {
		modelFlag = *models
	}
	defaultEffort = *effort
	turnTimeout = *timeout
	bootstrapPath, cookieFilePath = *bsFile, *cookieFile
	if txt, err := readTextFile(*promptFile); err == nil && txt != "" {
		prismSystemPrompt = txt
		log.Printf("已载入 Prism 系统提示词（%d 字符）", len(prismSystemPrompt))
	}

	if *bsFile != "" {
		txt, err := readTextFile(*bsFile)
		if err != nil {
			log.Fatalf("读 bootstrap 失败: %v", err)
		}
		if err := json.Unmarshal([]byte(txt), &bootstrap); err != nil {
			log.Fatalf("解析 bootstrap 失败: %v", err)
		}
		if bootstrap.SandboxURL == "" {
			bootstrap.SandboxURL = baseURL + "/s/sandboxes/proxy/"
		}
		log.Printf("bootstrap: conversation=%s sandbox_token=%d 字节 listen_snapshot=%v",
			bootstrap.ConversationID, len(bootstrap.SandboxToken), bootstrap.ListenSnapshot != "")
	}

	s := &session{}
	if *cookieFile != "" {
		if txt, err := readTextFile(*cookieFile); err == nil && txt != "" {
			s.cookie = txt
			log.Printf("已载入 cookie（%d 字节）", len(s.cookie))
		}
	}
	if bootstrap.Cookie != "" {
		s.cookie = bootstrap.Cookie
	}

	// 只暴露 UI 需要的三个静态文件：不能把 cookie.txt / bootstrap.json 一起端出去
	static := map[string]bool{"/app.css": true, "/app.js": true}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			http.ServeFile(w, r, *html)
		case static[r.URL.Path]:
			http.ServeFile(w, r, filepath.Join(filepath.Dir(*html), r.URL.Path))
		default:
			http.NotFound(w, r)
		}
	})
	http.HandleFunc("/api/start", s.handleStart)
	http.HandleFunc("/api/status", s.handleStatus)
	http.HandleFunc("/api/heartbeat", s.handleHeartbeat)
	http.HandleFunc("/api/state", s.handleState)
	http.HandleFunc("/api/bootstrap", s.handleSaveBootstrap)

	// 兼容接口：OpenAI / Anthropic 三种调用方式 + 模型列表
	http.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	http.HandleFunc("/v1/messages", s.handleMessages)
	http.HandleFunc("/v1/responses", s.handleResponses)
	http.HandleFunc("/v1/models", s.handleModels)
	http.HandleFunc("/v1/models/", s.handleModels)

	log.Printf("网页 UI: http://%s/", *addr)
	log.Printf("兼容接口: /v1/chat/completions, /v1/messages, /v1/responses, /v1/models")
	log.Fatal(http.ListenAndServe(*addr, nil))
}
