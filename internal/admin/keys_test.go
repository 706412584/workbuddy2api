package admin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSetTopLevelValueKeepsRestByteIdentical 是这套文本手术存在的意义：
// 改一个键不得动到文件里的任何其他字节。
//
// 若改用 JSON 往返（parse → 改 → 序列化），这条必然失败 —— 缩进、键序、
// 空行、注释式分组全会被重排。
func TestSetTopLevelValueKeepsRestByteIdentical(t *testing.T) {
	// 刻意写成「人维护的」样子：自定义缩进、键序、无关的嵌套结构
	src := "{\r\n" +
		"    \"listen\": \":7863\",\r\n" +
		"    \"cooldown\": {\r\n" +
		"        \"soft_rate\": \"600s\"\r\n" +
		"    },\r\n" +
		"    \"api_key\": \"old-value\",\r\n" +
		"    \"schedule\": {\r\n" +
		"        \"checkin_hours\": [9, 21]\r\n" +
		"    }\r\n" +
		"}\r\n"

	got, err := setTopLevelValue(src, "api_key", "new-value")
	if err != nil {
		t.Fatalf("setTopLevelValue: %v", err)
	}
	if !strings.Contains(got, `"api_key": "new-value"`) {
		t.Errorf("新值没写进去:\n%s", got)
	}
	// 除了这一处，其余字节必须原样
	want := strings.Replace(src, `"api_key": "old-value"`, `"api_key": "new-value"`, 1)
	if got != want {
		t.Errorf("除目标键外还有改动。\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	// CRLF 必须沿用：混入 \n 会写出混合行尾
	if strings.Count(got, "\r\n") != strings.Count(src, "\r\n") {
		t.Errorf("行尾风格被改变：CRLF %d → %d", strings.Count(src, "\r\n"), strings.Count(got, "\r\n"))
	}
	if len(got) != len(src)-len("old-value")+len("new-value") {
		t.Errorf("长度变化不符，说明有别处被重写: %d", len(got))
	}
}

// TestSetTopLevelValueNoHTMLEscaping 守住 Go 与 JS 序列化的一个致命差异：
// json.MarshalIndent 默认把 < > & 转成 \u003c 之类，JS 的 JSON.stringify 不转。
//
// 一旦转义，写入前的一致性断言就会判定「编辑结果与预期不一致」而放弃写入 ——
// 表现为「改密钥永远失败」，且错误信息完全指不到真正的原因。
func TestSetTopLevelValueNoHTMLEscaping(t *testing.T) {
	src := `{"api_keys": []}`
	got, err := setTopLevelValue(src, "api_keys", []KeyEntry{
		{Key: "k1", Region: "cn", Name: "<b>&amp;</b> 密钥"},
	})
	if err != nil {
		t.Fatalf("setTopLevelValue: %v", err)
	}
	for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(got, esc) {
			t.Errorf("出现 HTML 转义 %s，会让一致性断言失败:\n%s", esc, got)
		}
	}
	if !strings.Contains(got, "<b>&amp;</b> 密钥") {
		t.Errorf("原文字符没保留:\n%s", got)
	}
}

// TestSetTopLevelValueAppendsMissingKey 键不存在时追加到根对象末尾，
// 且不破坏原有成员（末尾逗号的有无都要处理对）。
func TestSetTopLevelValueAppendsMissingKey(t *testing.T) {
	cases := map[string]string{
		"已有成员":    `{"listen": ":7863"}`,
		"已有成员带逗号": `{"listen": ":7863",}`,
		"空对象":     `{}`,
		"空对象带空白":  "{  }",
		"已有成员多行":  "{\n  \"listen\": \":7863\"\n}",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := setTopLevelValue(src, "api_keys", []KeyEntry{})
			if err != nil {
				t.Fatalf("setTopLevelValue: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(got), &m); err != nil {
				t.Fatalf("结果不是合法 JSON: %v\n%s", err, got)
			}
			if _, ok := m["api_keys"]; !ok {
				t.Errorf("api_keys 没被追加:\n%s", got)
			}
			if strings.Contains(src, "listen") {
				if m["listen"] != ":7863" {
					t.Errorf("原有成员被破坏: %v\n%s", m["listen"], got)
				}
			}
		})
	}
}

// TestFindTopLevelValueIgnoresNestedAndStringValues 只认顶层成员。
// 同名的嵌套键或字符串值都不能被误当成目标。
func TestFindTopLevelValueIgnoresNestedAndStringValues(t *testing.T) {
	src := `{"note": "the api_key is secret", "inner": {"api_key": "nested"}, "api_key": "top"}`
	start, end, ok := findTopLevelValue(src, "api_key")
	if !ok {
		t.Fatal("没找到顶层的 api_key")
	}
	if got := src[start:end]; got != `"top"` {
		t.Errorf("定位到了错误的值: %s", got)
	}
}

// TestSetTopLevelValueQuotedKeysWithEscapes 值里含引号与反斜杠时，
// 扫描器必须正确跳过转义，不能把字符串里的 } 当成结构收尾。
func TestSetTopLevelValueQuotedKeysWithEscapes(t *testing.T) {
	src := `{"other": {"a": "}"}, "api_key": "旧值"}`
	got, err := setTopLevelValue(src, "api_key", `新"值\带转义`)
	if err != nil {
		t.Fatalf("setTopLevelValue: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("结果不是合法 JSON（转义处理有问题）: %v\n%s", err, got)
	}
	if m["api_key"] != `新"值\带转义` {
		t.Errorf("值不符: %v", m["api_key"])
	}
	if inner, _ := m["other"].(map[string]any); inner["a"] != "}" {
		t.Errorf("相邻结构被破坏: %v", m["other"])
	}
}

// TestSetTopLevelValueRejectsBrokenConfig 结构异常时返回错误而不是写出半截文件。
func TestSetTopLevelValueRejectsBrokenConfig(t *testing.T) {
	for name, src := range map[string]string{
		"没有根对象": "[1,2,3]",
		"空文本":   "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := setTopLevelValue(src, "api_key", "x"); err == nil {
				t.Error("结构异常时应报错")
			}
		})
	}
}

// TestValidateKeys 镜像 cmd/server/config.go 的校验：能通过的必须真能启动网关，
// 不能通过的必须被挡在写入之前。
func TestValidateKeys(t *testing.T) {
	cases := []struct {
		name   string
		legacy string
		keys   []KeyEntry
		want   string // 期望错误里包含的关键词；空 = 应当通过
	}{
		{"空列表通过（鉴权由调用方的二次确认把关）", "", nil, ""},
		{"仅 legacy", "abc", nil, ""},
		{"normal", "", []KeyEntry{{Key: "k1", Region: "cn"}, {Key: "k2", Region: "global"}}, ""},
		{"区域可空", "", []KeyEntry{{Key: "k1"}}, ""},
		{"空密钥", "", []KeyEntry{{Key: "  "}}, "不得为空"},
		{"非法区域", "", []KeyEntry{{Key: "k1", Region: "us"}}, "只能是 cn 或 global"},
		{"与 legacy 重复", "dup", []KeyEntry{{Key: "dup"}}, "重复"},
		{"彼此重复", "", []KeyEntry{{Key: "same"}, {Key: "same"}}, "重复"},
		{"大小写不同的区域合法", "", []KeyEntry{{Key: "k1", Region: "CN"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validateKeys(c.legacy, c.keys)
			if c.want == "" {
				if got != "" {
					t.Errorf("应通过，却报错: %s", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("错误信息不含 %q: %q", c.want, got)
			}
		})
	}
}

// TestToAnyMatchesUnmarshaledShape 深比较的两边必须是同一套类型。
// json.Unmarshal 得到的是 []any/map[string]any，而 keys 是 []KeyEntry ——
// 不先用 toAny 归一，断言会恒为不等，保存永远失败。
func TestToAnyMatchesUnmarshaledShape(t *testing.T) {
	keys := []KeyEntry{{Key: "k", Region: "cn", Name: "n"}}
	text, err := json.Marshal(map[string]any{"api_keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(text, &parsed); err != nil {
		t.Fatal(err)
	}
	if !deepEqualAny(parsed["api_keys"], toAny(keys)) {
		t.Errorf("toAny 归一后仍不相等:\n parsed=%#v\n toAny=%#v", parsed["api_keys"], toAny(keys))
	}
}

// deepEqualAny 测试内的深比较（reflect.DeepEqual 的薄封装，避免测试里直接依赖它）。
func deepEqualAny(a, b any) bool {
	ra, err1 := json.Marshal(a)
	rb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ra) == string(rb)
}
