"""从 prism 的 HAR 里抠出 bootstrap（sandbox_token / codex_listen_snapshot / ids）。

用法：
    python grab_token.py [har路径] [输出路径]
    # 默认 har = ~/Downloads/capture.har，输出 bootstrap.json

HAR 里不含 cookie（浏览器导出时已脱敏），cookie 需要自己贴到网页 UI 里，
或写进文件用 -cookie-file 传入。
"""
import json
import os
import sys

har_path = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
    os.path.expanduser("~"), "Downloads", "capture.har")
out_path = sys.argv[2] if len(sys.argv) > 2 else "bootstrap.json"

with open(har_path, encoding="utf-8") as f:
    har = json.load(f)

start_req = None
for e in har["log"]["entries"]:
    if e["request"]["url"].endswith("response_with_tools_start"):
        start_req = json.loads(e["request"]["postData"]["text"])
        break

if start_req is None:
    sys.exit("这个 HAR 里没有 response_with_tools_start 请求 —— 请先真的在页面上发一条消息再导出")

md = start_req.get("metadata", {})


def as_obj(v):
    """metadata 里的 listen_snapshot 是「JSON 字符串」，转回对象，方便嵌进请求体。"""
    if isinstance(v, dict):
        return v
    if isinstance(v, str) and v.strip():
        try:
            return json.loads(v)
        except Exception:
            return v
    return None


snap = as_obj(md.get("codex_listen_snapshot"))
conv = start_req.get("conversationId") or (snap or {}).get("conversation_id")

bs = {
    "sandbox_token": md.get("sandbox_token", ""),
    "sandbox_url": md.get("sandbox_url", "https://prism.openai.com/s/sandboxes/proxy/"),
    "project_id": md.get("projectId", (snap or {}).get("project_id", "")),
    "user_id": md.get("userId", (snap or {}).get("user_id", "")),
    "conversation_id": conv or "",
    "listen_snapshot": json.dumps(snap, ensure_ascii=False) if snap else "",
    "cookie": "",
}

with open(out_path, "w", encoding="utf-8") as f:
    json.dump(bs, f, ensure_ascii=False, indent=2)

print("已写出 %s" % out_path)
print("  sandbox_token : %d 字节 %s" % (len(bs["sandbox_token"]), "OK" if bs["sandbox_token"] else "缺失!"))
print("  listen_snapshot: %d 字节 %s" % (len(bs["listen_snapshot"]), "OK" if bs["listen_snapshot"] else "缺失!"))
print("  conversation_id: %s" % (bs["conversation_id"] or "(空)"))
print("  project/user   : %s / %s" % (bs["project_id"], bs["user_id"]))
if not bs["sandbox_token"]:
    print("\n警告：没有 sandbox_token，请求只会返回 sandbox_reconnecting。")
