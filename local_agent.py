"""local_agent.py —— prismctl 桥接模式（-local-tools）的**本机 agent 参考实现**。

架构：
    本机 agent（本文件）声明工具 → 网关转给模型 → 模型用 JSON 下单
    → 网关翻成标准 tool_calls → 本机 agent 真正执行 → 结果回灌 → 模型继续/作答

注意：工具是**客户端**定义的，网关不内置任何工具、也不判断命令安全性。
权限策略全部在本文件里，你可以按需改。

用法：
    python local_agent.py --root "C:\\Users\\me\\Desktop\\proj" \
        --task "在当前目录创建 1.html，内容是最简单的 HTML，标题写 测试" --yes

选项：
    --root DIR            文件操作的根目录（默认当前目录）；所有文件读写必须落在它里面
    --read-only           禁止一切写操作（建文件/改文件/删文件）
    --allow-shell-write   允许 run_shell 执行写命令（默认只放行只读命令）
    --allow-delete        允许删除文件
    --yes                 不再逐个确认（非交互场景用）
    --max-iters N         最多几轮工具循环（默认 12）
"""
import argparse
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request

DEFAULT_URL = "http://127.0.0.1:8899"
TOOL_TIMEOUT = 120

# ---------- 权限策略 ----------

READ_OK_HEADS = {
    "get-childitem", "gci", "dir", "ls", "get-content", "cat", "type", "get-item",
    "test-path", "get-date", "whoami", "hostname", "get-location", "pwd", "echo",
    "get-command", "get-process", "resolve-path", "measure-object", "select-string",
}
PIPE_OK_HEADS = {
    "select-object", "select", "where-object", "where", "sort-object", "sort",
    "format-table", "ft", "format-list", "fl", "measure-object", "measure",
    "out-string", "head", "select-string", "sls", "group-object", "uniq",
}
# 无条件拒绝（即使开了 --allow-shell-write）
FORBIDDEN = [
    "format ", "format-volume", "diskpart", "shutdown", "stop-computer", "restart-computer",
    "bcdedit", "vssadmin", "cipher /w", "reg delete", "takeown", "icacls",
    "rm -rf /", "del /f /s /q c:", "del /s /q c:", "remove-item -recurse c:",
    "remove-item -r c:", "net user", "net localgroup", "schtasks /create",
    "set-mppreference", "new-localuser", "add-localuser",
]


def shell_policy(command: str) -> str:
    """返回 '' 表示放行，否则返回拒绝原因。"""
    c = command.strip()
    if not c:
        return "空命令"
    low = c.lower()
    for bad in FORBIDDEN:
        if bad in low:
            return "命中禁止模式 %r" % bad
    if ALLOW_SHELL_WRITE:
        return ""
    for i, seg in enumerate(s.strip() for s in c.split("|")):
        head = re.split(r"\s+", seg, 1)[0].lower() if seg else ""
        if head not in (READ_OK_HEADS if i == 0 else PIPE_OK_HEADS):
            return ("默认只放行只读命令，第 %d 段以 %r 开头不被允许；"
                    "需要写操作用 write_file/append_file，或加 --allow-shell-write" % (i + 1, head))
    return ""


def safe_path(p: str) -> str:
    """把路径限制在 --root 内，防目录穿越。"""
    if not p:
        raise ValueError("路径为空")
    cand = p if os.path.isabs(p) else os.path.join(ROOT, p)
    q = os.path.realpath(cand)
    if os.path.normcase(q) != os.path.normcase(ROOT) and \
       os.path.commonpath([os.path.normcase(ROOT), os.path.normcase(q)]) != os.path.normcase(ROOT):
        raise ValueError("拒绝：路径 %s 超出允许的根目录 %s" % (q, ROOT))
    return q


# ---------- 工具实现 ----------

def t_list_dir(path="."):
    d = safe_path(path)
    if not os.path.isdir(d):
        return "[错误] 不是目录: %s" % d
    out = []
    for name in sorted(os.listdir(d)):
        full = os.path.join(d, name)
        if os.path.isdir(full):
            out.append("[DIR ] %s" % name)
        else:
            out.append("[FILE] %-40s %8d B" % (name, os.path.getsize(full)))
    return "\n".join(out) or "(空目录)"


def t_read_file(path, max_bytes=200000):
    f = safe_path(path)
    if not os.path.isfile(f):
        return "[错误] 文件不存在: %s" % f
    with open(f, "r", encoding="utf-8", errors="replace") as fh:
        data = fh.read(max_bytes + 1)
    if len(data) > max_bytes:
        data = data[:max_bytes] + "\n…(已截断)"
    return data


def t_write_file(path, content, mode="overwrite"):
    if READ_ONLY:
        return "[拒绝] 已启用 --read-only"
    f = safe_path(path)
    os.makedirs(os.path.dirname(f) or ".", exist_ok=True)
    with open(f, "a" if mode == "append" else "w", encoding="utf-8") as fh:
        fh.write(content or "")
    n = os.path.getsize(f)
    return "[OK] %s 已写入 %s（%d 字节）" % (mode, f, n)


def t_make_dir(path):
    if READ_ONLY:
        return "[拒绝] 已启用 --read-only"
    d = safe_path(path)
    os.makedirs(d, exist_ok=True)
    return "[OK] 目录已就绪: %s" % d


def t_delete_file(path):
    if READ_ONLY or not ALLOW_DELETE:
        return "[拒绝] 删除需要同时不启用 --read-only 且加 --allow-delete"
    f = safe_path(path)
    if not os.path.isfile(f):
        return "[错误] 文件不存在: %s" % f
    os.remove(f)
    return "[OK] 已删除 %s" % f


def t_run_shell(command, timeout=60):
    why = shell_policy(command)
    if why:
        return "[拒绝] %s" % why
    try:
        p = subprocess.run(["powershell", "-NoProfile", "-Command", command],
                           capture_output=True, text=True, timeout=timeout,
                           encoding="utf-8", errors="replace")
        out = ((p.stdout or "") + (p.stderr or "")).strip()
        return ("exit=%d\n" % p.returncode + out)[:4000] or "(无输出)"
    except subprocess.TimeoutExpired:
        return "[超时] 命令超过 %ds" % timeout
    except Exception as e:
        return "[执行失败] %r" % (e,)


TOOLS = [
    {"type": "function", "function": {"name": "list_dir",
     "description": "列出本机某个目录下的文件与子目录",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": []}}},
    {"type": "function", "function": {"name": "read_file",
     "description": "读取本机某个文本文件的完整内容",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}},
                    "required": ["path"]}}},
    {"type": "function", "function": {"name": "write_file",
     "description": "在本机创建或覆盖一个文本文件（会自动建父目录）",
     "parameters": {"type": "object", "properties": {
         "path": {"type": "string", "description": "相对 --root 的路径"},
         "content": {"type": "string"},
         "mode": {"type": "string", "enum": ["overwrite", "append"], "default": "overwrite"}},
         "required": ["path", "content"]}}},
    {"type": "function", "function": {"name": "make_dir",
     "description": "在本机创建目录",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}},
                    "required": ["path"]}}},
    {"type": "function", "function": {"name": "delete_file",
     "description": "删除本机一个文件（需显式授权）",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}},
                    "required": ["path"]}}},
    {"type": "function", "function": {"name": "run_shell",
     "description": "在本机 PowerShell 执行命令（默认只放行只读命令）",
     "parameters": {"type": "object", "properties": {
         "command": {"type": "string"}, "timeout": {"type": "integer"}},
         "required": ["command"]}}},
]

DISPATCH = {
    "list_dir": t_list_dir,
    "read_file": t_read_file,
    "write_file": t_write_file,
    "make_dir": t_make_dir,
    "delete_file": t_delete_file,
    "run_shell": t_run_shell,
}


def execute(name, args):
    fn = DISPATCH.get(name)
    if not fn:
        return "[错误] 未实现的工具 %s" % name
    if not AUTO_YES:
        print("\n  ⚠ 即将执行 %s %s" % (name, json.dumps(args, ensure_ascii=False)[:300]))
        try:
            if input("    允许? [y/N] ").strip().lower() not in ("y", "yes"):
                return "[用户拒绝]"
        except EOFError:
            return "[用户拒绝]"
    try:
        return fn(**args)
    except TypeError as e:
        return "[参数错误] %s" % e
    except Exception as e:
        return "[执行异常] %r" % (e,)


def chat(messages):
    body = json.dumps({"model": MODEL, "messages": messages, "tools": TOOLS},
                      ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(URL + "/v1/chat/completions", data=body, method="POST")
    req.add_header("content-type", "application/json; charset=utf-8")
    try:
        with urllib.request.urlopen(req, timeout=600) as r:
            return json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        ctx = e.read().decode("utf-8", "replace")
        sys.exit("网关返回 %d: %s" % (e.code, ctx[:500]))


def main():
    global ROOT, READ_ONLY, ALLOW_SHELL_WRITE, ALLOW_DELETE, AUTO_YES, URL, MODEL
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=".", help="文件操作根目录")
    ap.add_argument("--url", default=DEFAULT_URL)
    ap.add_argument("--model", default="gpt-6-astra")
    ap.add_argument("--task", required=True, help="交给模型的任务")
    ap.add_argument("--max-iters", type=int, default=12)
    ap.add_argument("--read-only", action="store_true")
    ap.add_argument("--allow-shell-write", action="store_true")
    ap.add_argument("--allow-delete", action="store_true")
    ap.add_argument("--yes", action="store_true")
    a = ap.parse_args()

    ROOT = os.path.realpath(a.root)
    READ_ONLY, ALLOW_SHELL_WRITE = a.read_only, a.allow_shell_write
    ALLOW_DELETE, AUTO_YES = a.allow_delete, a.yes
    URL, MODEL = a.url.rstrip("/"), a.model

    if not os.path.isdir(ROOT):
        sys.exit("根目录不存在: %s" % ROOT)
    print("根目录 : %s" % ROOT)
    print("写权限 : %s ｜ shell 写: %s ｜ 删除: %s" %
          ("只读" if READ_ONLY else "允许", ALLOW_SHELL_WRITE, ALLOW_DELETE))

    messages = [
        {"role": "system", "content":
         "你是运行在用户 Windows 本机上的助手，通过工具直接操作他的电脑。"
         "需要看文件用 read_file/list_dir，需要写或改文件用 write_file（mode=append 追加），"
         "需要执行命令用 run_shell。不要凭空猜测文件内容，先读再改。"},
        {"role": "user", "content": a.task},
    ]

    for it in range(1, a.max_iters + 1):
        resp = chat(messages)
        ch = (resp.get("choices") or [{}])[0]
        msg = ch.get("message") or {}
        calls = msg.get("tool_calls") or []
        if not calls:
            print("\n=== 最终回答 ===")
            print(msg.get("content") or "(空)")
            return
        messages.append({"role": "assistant", "content": msg.get("content"), "tool_calls": calls})
        for c in calls:
            name = (c.get("function") or {}).get("name") or ""
            raw = (c.get("function") or {}).get("arguments") or "{}"
            try:
                args = json.loads(raw)
            except Exception:
                args = {}
            print("\n[轮次 %d] 工具 %s" % (it, name))
            out = execute(name, args)
            for line in str(out).splitlines()[:15]:
                print("   | " + line)
            messages.append({"role": "tool", "tool_call_id": c.get("id"),
                             "content": "[tool_result]\n" + str(out)[:6000]})
    print("\n达到最大轮次 %d，停止" % a.max_iters)


if __name__ == "__main__":
    main()
