# prismctl

用**浏览器 Cookie + sandbox bootstrap** 驱动 `prism.openai.com`，对外提供两套东西：

1. **网页调试台**（`http://127.0.0.1:8899/`）：左侧历史回合 / 工具调用 / 事件流，右侧答案与原始 JSON。
2. **兼容接口**：`/v1/chat/completions`、`/v1/messages`、`/v1/responses`、`/v1/models` ——
   现有 OpenAI / Anthropic SDK 改个 base_url 就能直接用。

> 逆向自网页端私有接口，无官方支持、无版本承诺，前端改字段就可能失效。仅供个人调试研究。

## 实测确认的上游链路（2026-09）

| 调用 | 作用 | 关键点 |
|---|---|---|
| `GET /s/sandboxes/proxy/heartbeat?prism_cache_bust=<ms>` | 沙箱探活/预热 | 头 `x-crixet-sandbox-token` |
| `POST /api/llm/response_with_tools_start` | 发起一轮 | `metadata` 必须带 `sandbox_token` + `codex_listen_snapshot`，且 `conversationId` 要和它们配套 |
| `POST /api/llm/response_with_tools_status` | 轮询结果 | 必须把 start 回传的 `turn_state` **原样**回传，否则 401 |

**坑位记录**

1. Cookie 必须含 `oai-sc`、`prism_session_token`、`prism_oai_access_token`、`cf_clearance`、`__cf_bm`。
   少 `oai-sc` 会得到 `401 Could not parse your authentication token`。
2. 缺 `sandbox_token` → HTTP 200 但 `reason: "sandbox_reconnecting"`、`sandbox_token_present: false`，**永远等不到回答**。
3. `status` 不原样回传 `turn_state` → 401。
4. `toolCalls` / `eventPreviews` **只在 `pending` 期间回传**，`completed` 时为空，必须边轮询边累积（网关默认 1.2s 轮询）。
5. `cf_clearance` 与出口 IP + User-Agent 绑定，代码里 UA 固定 Chrome/151；换网络或 UA 会 403。
6. Cloudflare 上 Go 的 HTTP/2 连接复用会偶发 `PROTOCOL_ERROR`，客户端已改为 HTTP/1.1 并对传输层错误重试。

## 兼容接口

### 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/models`、`/v1/models/{id}` | 模型列表（`gpt-6-astra` 及其 effort 变体） |
| POST | `/v1/chat/completions` | OpenAI Chat Completions，支持 `stream` |
| POST | `/v1/messages` | Anthropic Messages，支持 `stream` |
| POST | `/v1/responses` | OpenAI Responses API，支持 `stream` |

模型名可把 `reasoning_effort` 编码进去：`gpt-6-astra-high`、`gpt-6-astra-xhigh`（默认）、`-medium/-low/-minimal`。

### 用现有 SDK

```bash
# OpenAI
export OPENAI_BASE_URL=http://127.0.0.1:8899/v1
export OPENAI_API_KEY=sk-anything          # 网关不校验 key

# Anthropic
export ANTHROPIC_BASE_URL=http://127.0.0.1:8899
export ANTHROPIC_API_KEY=sk-anything       # 注意 ANTHROPIC_BASE_URL 不带 /v1
```

```bash
curl http://127.0.0.1:8899/v1/chat/completions -H 'content-type: application/json' -d '{
  "model":"gpt-6-astra-high","stream":true,
  "messages":[{"role":"system","content":"回答尽量简短"},
              {"role":"user","content":"列出项目里的文件"}]}'

curl http://127.0.0.1:8899/v1/messages -H 'content-type: application/json' -d '{
  "model":"gpt-6-astra-high","max_tokens":1024,"stream":true,
  "system":"回答尽量简短",
  "messages":[{"role":"user","content":[{"type":"text","text":"main.tex 有多少行？"}]}]}'

curl http://127.0.0.1:8899/v1/responses -H 'content-type: application/json' -d '{
  "model":"gpt-6-astra-high","stream":true,
  "instructions":"回答尽量简短","input":"main.tex 有多少行？"}'
```

### 请求翻译规则

```
调用方 messages
  ├─ 全部 system 合并        ┐
  ├─ 之前的历史对话           ├→ prism 的 input[0] system
  ├─ 调用方声明的 tools 清单   ┘   （前面还会拼上 Prism 官方系统提示词）
  └─ 最后一条非 system 消息   → prism 的 input[1] user
```

`conversationId` 默认复用 bootstrap 里的那个，因此沙箱会话（cwd、文件）在多轮之间是连续的。

### 工具调用怎么返回

沙箱执行工具时（`exec_command` 等）会**即时**按各协议格式流出去，随后再补最终文本：

- **OpenAI**：`choices[].delta.tool_calls[]`（含 `id` / `function.name` / `function.arguments`），最后 `finish_reason: "stop"` + `data: [DONE]`
- **Anthropic**：`content_block_start`(`tool_use`) → `content_block_delta`(`input_json_delta`) → `content_block_stop`，然后才是文本块
- **Responses**：`response.output_item.added`(`function_call`) → `response.function_call_arguments.delta` → `response.output_item.done`，`response.completed` 里同时带 `function_call` 与 `message` item

**注意**：这些是**沙箱自己执行的**工具调用（已经在服务端跑完了），流给你是让你看见过程。
不要把流出的 `tool_calls` 再拿去执行一遍；`tools` 字段默认只作为提示词注入，网关不代理你的工具执行。

### 客户端工具桥接（`-local-tools`）

想让模型操作**你本机**（而不是那个 Linux 沙箱）时用这个开关。

先说清楚上游的事实（已实测）：

| 实验 | 结果 |
|---|---|
| 网页端 `start` 请求体顶层字段 | 只有 `input` / `metadata` / `conversationId`，**没有 `tools`** |
| 手动往请求里塞 `tools:[{name:"echo_local",…}]` 并让它调用 | 模型回 `no_tool_available` → **该字段被忽略** |
| 模型实际用过的工具 | `exec_command` 等，全部是服务端 Codex agent 内置的 |

也就是说：**上游没有客户端 function calling 通道**，工具清单在服务端，作用域在沙箱容器里。
桥接模式用"模型用 JSON 下单"绕过这一点：

```
模型输出: {"tool_calls":[{"name":"run_local","arguments":{"command":"…"}}]}
   ↓ 网关解析成真正的 tool_calls / tool_use（finish_reason=tool_calls / stop_reason=tool_use）
本机 agent 在你自己机器上执行
   ↓ 结果以 role:"tool" 或 tool_result 回灌下一轮
模型看到真实输出，继续或作答
```

启用：

```bash
go run main.go -addr 127.0.0.1:8899 -bootstrap bootstrap.json -cookie-file cookie.txt -local-tools
```

行为差异：

- 请求**声明了 `tools`** 时，注入代理协议、固定 `low` 推理强度、且不注入 Prism 人格提示词
  （否则"用你自己的工具"和"只用 JSON 下单"两条指令会打架）
- 未声明 `tools` 的请求模型行为不变；但**只要它输出了形如工具调用的 JSON 也会被转换**
  （避免客户端漏传 `tools` 时把 JSON 漏成一段纯文本）
- 工具名做**归一化匹配**（忽略大小写与 `-`/`_`/空格）；模型用了未声明的名字也照样按工具调用返回并记日志，
  **不再静默丢弃**
- **输出解析很宽容**：代码块（含语言标记）、前后夹带说明文字、参数里嵌套花括号、markdown 加粗、
  中文弯引号、尾逗号、`arguments` 双重编码，以及 `name`/`tool`/`tool_name`/`function` 与
  `arguments`/`args`/`input`/`parameters` 各种键名写法都能识别（见 `gateway_test.go`）
- 另外两个协议同样支持：`/v1/messages` 会给 `stop_reason: tool_use`、`/v1/responses` 走 `function_call` item

#### 已实测可用：Codex CLI（推荐）

| 客户端 | 请求里声明 tools？ | 结果 |
|---|---|---|
| **Codex CLI**（`codex exec`） | ✅ 15 个（`exec_command`、`write_stdin`、`view_image`、MCP 工具…） | **全链路可用** |
| Codex 桌面版 App | ❌ `tools: null`（它走 `tool_mode` 的"代码模式"，不下发 function tools） | 模型只能用自己的沙箱 |

桌面版那条路会让人误以为"AI 建好了文件"——其实建在 `/codex_workspace/<会话>` 里（Linux 容器），
跟你电脑无关。**请用 CLI。**

```powershell
codex exec --skip-git-repo-check -C "C:\path\to\project" -s workspace-write "在当前目录创建 x.html，内容是最简 HTML"
```

实测（本机 Windows，两次）：

```
codex exec … -Command "Get-Location; if (Test-Path …)" in C:\…\proj   succeeded in 374ms
… 模型拿到结果后改写命令 …   succeeded in 265ms
最终回答：「已创建并确认 x2.html 存在，标题为"第二次测试"，文件大小为 106 字节。」
磁盘核实：x2.html  106 字节 ✅
```

两个必须知道的点：

- `-s workspace-write` 让 Codex 允许写工作目录；**不带就只能读**（默认 `read-only`）。
- `-C <目录>` 指定工作目录。路径不在工作区内会被 Codex 沙箱拒绝。
- 模型首次常会尝试把 `apply_patch` 补丁嵌进 PowerShell（引号转义必失败）——协议里已加
  "Windows 上直接用 `Set-Content -LiteralPath … -Encoding UTF8`" 的指引，实测第二次就一次成功。

实测（本机 Windows，`local_agent.py` 客户端）：

```
第 1 轮 finish_reason=tool_calls
  run_local {"command":"Get-ChildItem -LiteralPath 'C:\\Users\\me\\Desktop\\proj' -Force"}
  本机输出 → d----- src／-a---- 32027 index.html
第 2 轮 finish_reason=stop
  最终回答「proj 包含 src（目录）和 index.html（文件，32,027 字节）」
```

**代价与风险**：靠模型守约输出 JSON，强模型大多能听话但会偶尔跑偏；执行命令的**权限控制必须由你的本机 agent 负责**——
网关只做协议翻译，不判断命令是否安全，千万别把未经审查的模型命令直接丢给 shell。
追求稳定建议本机 agent 直连真 API。

### 本机 agent 参考实现（`local_agent.py`）

桥接模式要求**客户端自己执行工具**。仓库里带了一个可直接用的实现：

```bash
python local_agent.py --root "C:\Users\me\Desktop\proj" \
  --task "在当前目录创建 1.html，内容是能双击打开的最简 HTML，标题写 测试" --yes
```

它向网关声明并实现六个**本机**工具：`list_dir` / `read_file` / `write_file`（overwrite|append）/
`make_dir` / `delete_file` / `run_shell`，并自带权限策略：

| 选项 | 作用 |
|---|---|
| `--root DIR` | 文件操作根目录，所有读写**必须**落在它里面（防目录穿越） |
| `--read-only` | 禁止一切写操作 |
| `--allow-shell-write` | 允许 `run_shell` 执行写命令（默认只放行只读命令） |
| `--allow-delete` | 允许删除文件 |
| `--yes` | 跳过逐个确认（默认每次执行前问你 y/N） |
| `--max-iters N` | 最多几轮工具循环，默认 12 |

实测（本机 Windows）：创建 `1.html`（157 B）→ 读回校验 → 按要求改成 224 B 并加段落与链接 → 再次读回确认。

**再次强调**：工具是**客户端**定义的，prismctl 不内置任何工具、不解析命令、也不做权限判断。
`local_agent.py` 只是参考实现，策略请按自己的风险偏好改。

### 已知限制

- `usage` 是**按字符数估算**的（上游不返回 token 用量）。
- `temperature` / `max_tokens` / `top_p` 等采样参数会被忽略（沙箱不接受这些）。
- 一轮最长等待由 `-turn-timeout` 控制（默认 8 分钟）；沙箱冷启动可能明显变慢。
- 没有实现 `tool_choice`、多模态输入、`n>1`、logprobs。
- **模型跑在 OpenAI 的远程 Linux 容器里**（`/codex_workspace/<会话id>`），它的工具调用是在那个容器里
  执行的，**没有任何通道能读到客户端本机**。所以别指望把 Claude Code / Codex CLI 指向本网关就能操作
  你本机文件——模型对自己的环境描述（"我看不到你的 C:\"）是对的。
  要操作本机请开 `-local-tools`（见上文桥接章节），由**本机 agent** 执行工具。
- `tools` 字段默认被忽略（实测：服务端不接受客户端自定义工具），只有 `-local-tools` 打开时才被解析。

## 依赖与运行

- Go 1.21+（纯标准库）；Python 3 仅用于 `grab_token.py` / `extract_prompt.py`

```bash
python grab_token.py ~/Downloads/capture.har bootstrap.json   # 沙箱凭据
python extract_prompt.py ~/Downloads/capture.har              # 可选：官方系统提示词
go run main.go -addr 127.0.0.1:8899 -bootstrap bootstrap.json -cookie-file cookie.txt
```

| 参数 | 说明 |
|---|---|
| `-addr` | 监听地址，默认 `127.0.0.1:8899` |
| `-bootstrap` | `grab_token.py` 生成的 `bootstrap.json` |
| `-cookie-file` | cookie 落盘路径；也可在网页 UI 里粘贴 |
| `-prism-prompt` | **默认关闭**。填 `prism_system_prompt.txt` 才注入官方提示词（模型会扮演网页端那个 LaTeX 助手，只认远端项目，看不到你本机） |
| `-models` | 逗号分隔的模型白名单 |
| `-effort` | 默认 `reasoning_effort`，默认 `xhigh` |
| `-turn-timeout` | 单轮最长等待，默认 `8m` |
| `-local-tools` | 客户端工具桥接：模型用 JSON 下单，网关翻成真正的 tool_calls 交本机执行（默认关闭） |

**安全**：网关**不校验任何 API key**，默认只绑 `127.0.0.1`。若改成 `-addr 0.0.0.0:8899`
等于把你的会话开放给整个网络，请自行在前面加鉴权/反代。

## 敏感数据

- `cookie.txt` / `bootstrap.json` **是凭据**（会话 token、sandbox_token、projectId/userId），
  `.gitignore` 已排除，**不要提交、不要分享**。
- 服务端只暴露 `index.html` / `app.css` / `app.js` 三个静态文件；`bootstrap.json`、`cookie.txt`
  即使同目录也不会被 HTTP 提供。
- 时效：`prism_session_token` 约 12 小时；`sandbox_token` 实测可用 1.5 小时以上，沙箱被回收后失效
  —— 重新在页面发一条消息、重导 HAR、重跑 `grab_token.py` 即可。

## 排查

| 现象 | 原因 |
|---|---|
| `401 Could not parse your authentication token` | cookie 不完整（最常见：漏 `oai-sc`），或 `cf_clearance` 与当前 IP/UA 不匹配 |
| 200 但 `sandbox_reconnecting` | 缺 `sandbox_token`（或 `conversationId` 与 snapshot 不配套） |
| `status` 401 | `turn_state` 没有原样回传 |
| 左侧工具/事件面板一直空 | 轮询太慢：这些数据只在 `pending` 期间出现（默认 1.2s 轮询） |
| 点按钮没反应 | 打开 F12 Console，页面也会把 JS 错误打在"原始 JSON"面板 |
| 上游 `PROTOCOL_ERROR` | Cloudflare 上的 HTTP/2 连接复用问题，已改 HTTP/1.1 + 重试 |
| `沙箱侧 504 Gateway Timeout: … workspace file synchronization` | 沙箱为这个会话恢复工作区超时：回浏览器重新打开该项目（会重新分配沙箱），再导一次 HAR、重跑 `grab_token.py` |
| `messages 为空` | 请求里一条消息都没有；只有 system 的探活请求现在会自动用最后一条当 prompt |
