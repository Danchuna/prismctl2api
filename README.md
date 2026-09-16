# prismctl

用**浏览器 Cookie + sandbox bootstrap** 驱动 `prism.openai.com` 对话接口的小客户端：
本地网页 UI，带历史回合、工具调用、事件流调试面板。

> 逆向自网页端私有接口，无官方支持、无版本承诺，前端改字段就可能失效。仅供个人调试研究使用。

## 实测确认的链路（2026-09）

| 调用 | 作用 | 关键点 |
|---|---|---|
| `GET /s/sandboxes/proxy/heartbeat?prism_cache_bust=<ms>` | 沙箱探活/预热 | 请求头 `x-crixet-sandbox-token` |
| `POST /api/llm/response_with_tools_start` | 发起一轮 | `metadata` 必须带 `sandbox_token` + `codex_listen_snapshot`，且 `conversationId` 要和它们配套 |
| `POST /api/llm/response_with_tools_status` | 轮询结果 | 必须把 start 回传的 `turn_state` **原样**回传，否则 401 |

响应要点：

- `start` 成功 → `{"status":"started", "request_id", "turn_state", "codex_listen_snapshot"}`
- 轮询中 → `{"status":"pending", "codex_live_progress":{toolCalls, eventPreviews, ...}}`
- 完成 → `{"status":"completed", "response":{payload:{output[], codexDeltaFiles[], codexExecMeta}}}`
  - 回答正文：`response.payload.output[].content[].text`
  - 它改动的文件：`response.payload.codexDeltaFiles[]`

**坑位记录**

1. Cookie 必须含 `oai-sc`、`prism_session_token`、`prism_oai_access_token`、`cf_clearance`、`__cf_bm`。
   少了 `oai-sc` 会得到 `401 Could not parse your authentication token`。
2. 缺 `sandbox_token` → HTTP 200 但 `reason: "sandbox_reconnecting"`、`sandbox_token_present: false`，且**永远等不到回答**。
3. `status` 若不原样回传 `turn_state`（哪怕只填空串）→ 401。
4. `toolCalls` / `eventPreviews` **只在 `pending` 期间回传**，`completed` 时 `codex_live_progress` 为空，所以要边轮询边累积。
5. `cf_clearance` 与出口 IP + User-Agent 绑定；代码里 UA 已固定为 Chrome/151，换网络或换 UA 会 403。

## 依赖

- Go 1.21+（服务端，纯标准库）
- Python 3（仅 `grab_token.py` 用，解析 HAR）

## 快速开始

```bash
# 1) 拿到 bootstrap：在浏览器里正常用一次 Prism（打开项目 → 发一条消息），
#    DevTools → Network → 右键 → Save all as HAR，然后：
python grab_token.py ~/Downloads/prism.openai.com.har bootstrap.json

# 2) 写 cookie（从 DevTools 请求头整行复制）
#    存成 cookie.txt，或直接在网页 UI 的"配置"里粘贴

# 3) 启动
go run main.go -addr 127.0.0.1:8899 -bootstrap bootstrap.json -cookie-file cookie.txt
```

打开 <http://127.0.0.1:8899/> 即可提问（Ctrl+Enter 发送）。

## 参数

| 参数 | 说明 |
|---|---|
| `-addr` | 监听地址，默认 `127.0.0.1:8899`（只监听本机） |
| `-bootstrap` | `grab_token.py` 生成的 `bootstrap.json` |
| `-cookie-file` | cookie 落盘路径；也可在 UI 里粘贴 |
| `-html` | UI 文件，默认 `index.html` |

## 敏感数据

- `cookie.txt` / `bootstrap.json` **是凭据**（会话 token、sandbox_token、你的 projectId/userId），
  `.gitignore` 已排除，**不要提交、不要分享**。
- 服务端只暴露 `index.html` / `app.css` / `app.js` 三个静态文件，
  `bootstrap.json`、`cookie.txt` 即使放在同目录也不会被 HTTP 提供。
- 过期与刷新：`prism_session_token` 约 12 小时；`sandbox_token` 活得比预想久（实测 > 1.5 小时），
  沙箱被回收后失效 —— 重新在页面发一条消息、重导 HAR、重跑 `grab_token.py` 即可。

## 排查

| 现象 | 原因 |
|---|---|
| `401 Could not parse your authentication token` | cookie 不完整（最常见：漏 `oai-sc`），或 `cf_clearance` 与当前 IP/UA 不匹配 |
| 200 但 `sandbox_reconnecting` | 缺 `sandbox_token`（或 `conversationId` 与 snapshot 不配套） |
| `status` 401 | `turn_state` 没有原样回传 |
| 工具/事件面板一直是空 | 轮询太慢：这些数据只在 `pending` 期间出现（默认 1500ms 轮询） |
| 点按钮没反应 | 打开 F12 Console，页面也会把 JS 错误打在"原始 JSON"面板 |
