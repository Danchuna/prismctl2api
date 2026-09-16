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

var httpc = &http.Client{Transport: &http.Transport{
	ResponseHeaderTimeout: 300 * time.Second,
	IdleConnTimeout:       90 * time.Second,
}}

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
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, rdr)
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
	code, respBody, err := upstream(startPath, body, cookie, nil)
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
	code, respBody, err := upstream(statusPath,
		map[string]any{"request_id": rid, "turn_state": ts}, cookie, nil)
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
	code, respBody, err := upstream(hbPath+"?prism_cache_bust="+fmt.Sprint(time.Now().UnixMilli()),
		nil, cookie, map[string]string{"x-crixet-sandbox-token": f.SandboxToken})
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
	flag.Parse()

	if *bsFile != "" {
		b, err := os.ReadFile(*bsFile)
		if err != nil {
			log.Fatalf("读 bootstrap 失败: %v", err)
		}
		if err := json.Unmarshal(b, &bootstrap); err != nil {
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
		if b, err := os.ReadFile(*cookieFile); err == nil {
			s.cookie = strings.TrimSpace(string(b))
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
	log.Printf("网页 UI: http://%s/", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
