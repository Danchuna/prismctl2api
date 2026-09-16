"""从 HAR 的 start 请求体里抽出第一条 system 消息（Prism 官方系统提示词）。"""
import json
import os
import sys

har_path = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
    os.path.expanduser("~"), "Downloads", "capture.har")
out_path = sys.argv[2] if len(sys.argv) > 2 else "prism_system_prompt.txt"

with open(har_path, encoding="utf-8") as f:
    har = json.load(f)

start = None
for e in har["log"]["entries"]:
    if e["request"]["url"].endswith("response_with_tools_start"):
        start = json.loads(e["request"]["postData"]["text"])
        break
if start is None:
    sys.exit("HAR 里没有 start 请求")

for m in start.get("input", []):
    if m.get("role") != "system":
        continue
    text = (m.get("content") or [{}])[0].get("text", "")
    if len(text) > 500:  # 真正的系统提示词很长，上下文 JSON 那条较短
        with open(out_path, "w", encoding="utf-8") as f:
            f.write(text)
        print("已写出 %s（%d 字符）" % (out_path, len(text)))
        break
else:
    sys.exit("没找到像系统提示词的 system 消息")
