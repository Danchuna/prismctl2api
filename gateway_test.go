package main

import (
	"encoding/json"
	"testing"
)

var testTools = []toolDef{
	{Type: "function", Function: &toolFunction{Name: "list_dir"}},
	{Type: "function", Function: &toolFunction{Name: "read_file"}},
	{Type: "function", Function: &toolFunction{Name: "write_file"}},
	{Type: "function", Function: &toolFunction{Name: "run_shell"}},
}

func TestParseToolCalls(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantNames []string  // nil 表示应当解析不出工具调用
		wantArgs  []string  // 与 wantNames 一一对应，空串表示不校验
	}{
		{
			name:      "用户反馈的原样文本",
			input:     `{"tool_calls":[{"name":"list_dir","arguments":{"path":"."}}]}`,
			wantNames: []string{"list_dir"},
			wantArgs:  []string{`{"path":"."}`},
		},
		{
			name:      "裸 JSON 带结尾句号",
			input:     `{"tool_calls":[{"name":"list_dir","arguments":{"path":"."}}]}。`,
			wantNames: []string{"list_dir"},
		},
		{
			name:      "json 代码块",
			input:     "好的，我来看看。\n```json\n{\"tool_calls\":[{\"name\":\"list_dir\",\"arguments\":{\"path\":\".\"}}]}\n```",
			wantNames: []string{"list_dir"},
		},
		{
			name:      "大写语言标记的代码块",
			input:     "```JSON\n{\"tool_calls\":[{\"name\":\"list_dir\",\"arguments\":{}}]}\n```",
			wantNames: []string{"list_dir"},
		},
		{
			name:      "无语言标记的代码块",
			input:     "```\n{\"tool_calls\":[{\"name\":\"list_dir\",\"arguments\":{}}]}\n```",
			wantNames: []string{"list_dir"},
		},
		{
			name:      "正文里夹带花括号（后缀干扰）",
			input:     `{"tool_calls":[{"name":"list_dir","arguments":{"path":"."}}]}` + "\n（说明：{这不是 JSON}）",
			wantNames: []string{"list_dir"},
		},
		{
			name:      "markdown 加粗包裹",
			input:     `**{"tool_calls":[{"name":"list_dir","arguments":{}}]}**`,
			wantNames: []string{"list_dir"},
		},
		{
			name:      "中文弯引号",
			input:     `{“tool_calls”:[{“name”:“list_dir”,“arguments”:{“path”:“.”}}]}`,
			wantNames: []string{"list_dir"},
		},
		{
			name:      "尾逗号",
			input:     `{"tool_calls":[{"name":"list_dir","arguments":{"path":"."},}],}`,
			wantNames: []string{"list_dir"},
		},
		{
			name:      "arguments 双重编码",
			input:     `{"tool_calls":[{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}]}`,
			wantNames: []string{"read_file"},
			wantArgs:  []string{`{"path":"a.txt"}`},
		},
		{
			name:      "单对象 tool 键",
			input:     `{"tool":"list_dir","arguments":{"path":"."}}`,
			wantNames: []string{"list_dir"},
		},
		{
			name:      "单对象 name+args",
			input:     `{"name":"read_file","args":{"path":"a.txt"}}`,
			wantNames: []string{"read_file"},
		},
		{
			name:      "function 对象形式",
			input:     `{"tool_calls":[{"function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}`,
			wantNames: []string{"read_file"},
			wantArgs:  []string{`{"path":"a"}`},
		},
		{
			name:      "未声明的工具名也要返回（不能漏成纯文本）",
			input:     `{"tool_calls":[{"name":"do_whatever","arguments":{"x":1}}]}`,
			wantNames: []string{"do_whatever"},
		},
		{
			name:      "工具名归一化匹配（list-dir -> list_dir）",
			input:     `{"tool_calls":[{"name":"list-dir","arguments":{}}]}`,
			wantNames: []string{"list_dir"},
		},
		{
			name:      "大小写不同也能匹配",
			input:     `{"tool_calls":[{"name":"LIST_DIR","arguments":{}}]}`,
			wantNames: []string{"list_dir"},
		},
		{
			name:      "多个调用",
			input:     `{"tool_calls":[{"name":"list_dir","arguments":{"path":"."}},{"name":"read_file","arguments":{"path":"a"}}]}`,
			wantNames: []string{"list_dir", "read_file"},
		},
		{
			name:      "参数里含嵌套花括号",
			input:     `{"tool_calls":[{"name":"write_file","arguments":{"path":"a","content":"{\"a\":1}"}}]}`,
			wantNames: []string{"write_file"},
			wantArgs:  []string{`{"path":"a","content":"{\"a\":1}"}`},
		},
		{
			name:  "普通 JSON（不是工具调用）",
			input: `{"result":"ok","count":3}`,
		},
		{
			name:  "普通文本",
			input: "这个项目里有 index.html 和 1.txt，共 2 个文件。",
		},
		{
			name:  "空文本",
			input: "",
		},
	}

	for _, c := range cases {
		got := parseToolCalls(c.input, testTools)
		if len(c.wantNames) == 0 {
			if len(got) != 0 {
				t.Errorf("[%s] 期望解析不出工具调用，实际得到 %+v", c.name, got)
			}
			continue
		}
		if len(got) != len(c.wantNames) {
			t.Errorf("[%s] 期望 %d 个调用 %v，实际 %d 个 %+v",
				c.name, len(c.wantNames), c.wantNames, len(got), got)
			continue
		}
		for i, want := range c.wantNames {
			if got[i].Name != want {
				t.Errorf("[%s] 第 %d 个调用名期望 %q，实际 %q", c.name, i, want, got[i].Name)
			}
			if i < len(c.wantArgs) && c.wantArgs[i] != "" {
				var a, b any
				if json.Unmarshal([]byte(got[i].Args), &a) != nil || json.Unmarshal([]byte(c.wantArgs[i]), &b) != nil {
					t.Errorf("[%s] 第 %d 个参数不是合法 JSON: %q", c.name, i, got[i].Args)
				}
			} else if !json.Valid([]byte(got[i].Args)) {
				t.Errorf("[%s] 第 %d 个参数不是合法 JSON: %q", c.name, i, got[i].Args)
			}
		}
	}
}

func TestNormName(t *testing.T) {
	for in, want := range map[string]string{
		"list_dir": "listdir", "List-Dir": "listdir", " list dir ": "listdir",
	} {
		if got := normName(in); got != want {
			t.Errorf("normName(%q)=%q，期望 %q", in, got, want)
		}
	}
}
