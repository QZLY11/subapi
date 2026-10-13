package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================================
// 图片通道（保留 image_url 块 + 对象形态归一化）
// ============================================================================

// findImageURLPart 在 messages[0].content 数组中找 type=image_url 的块。
func findImageURLPart(t *testing.T, out []byte) (map[string]any, []any) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal out: %v (out=%s)", err, out)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages missing: %s", out)
	}
	msg, _ := msgs[0].(map[string]any)
	content, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content must stay an array when an image is present; got %T (%v)", msg["content"], msg["content"])
	}
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if ok && part["type"] == "image_url" {
			return part, content
		}
	}
	t.Fatal("image_url part not found (image was dropped)")
	return nil, nil
}

// TestWorkBuddyArrayContent_ImagePreserved 覆盖含图 content 数组的保留与
// image_url 归一化：
// 字符串形态必须转为上游需要的对象形态；对象形态及其中字段必须原样保留；
// 无效输入不补默认值，继续交给上游返回真实错误。
func TestWorkBuddyArrayContent_ImagePreserved(t *testing.T) {
	tests := []struct {
		name string
		body string
		want any // nil = image_url 键应缺失
	}{
		{
			name: "data url string to object",
			body: `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := PrepareWorkBuddyChatPayload([]byte(tc.body))
			part, _ := findImageURLPart(t, out)
			if tc.want == nil {
				if _, exists := part["image_url"]; exists {
					t.Fatalf("missing image_url should stay missing, got %#v", part)
				}
				return
			}
			got := part["image_url"]
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("image_url=%#v want %#v", got, tc.want)
			}
		})
	}
}

// TestWorkBuddyArrayContent_TextOnlyStillFolds 纯文本数组必须仍然折叠为字符串
// （腾讯上游对纯文本 content 数组会 400 code=11101，此行为不能回退）。
func TestWorkBuddyArrayContent_TextOnlyStillFolds(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`
	out := PrepareWorkBuddyChatPayload([]byte(body))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	content := msgs[0].(map[string]any)["content"]
	s, ok := content.(string)
	if !ok {
		t.Fatalf("text-only array must fold to string, got %T", content)
	}
	if s != "a\n\nb" {
		t.Fatalf("folded content = %q, want %q", s, "a\n\nb")
	}
}

// TestWorkBuddyArrayContent_ImageWithTextKeepsBoth 图文混合时文本与图片都不能丢。
func TestWorkBuddyArrayContent_ImageWithTextKeepsBoth(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"描述这张图"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`
	out := PrepareWorkBuddyChatPayload([]byte(body))
	_, content := findImageURLPart(t, out)
	var sawText bool
	for _, rawPart := range content {
		part, _ := rawPart.(map[string]any)
		if part["type"] == "text" && part["text"] == "描述这张图" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("text block lost from image message: %s", out)
	}
}

// TestSanitizeWorkBuddyBrandWords_ArrayText 含图消息的文本块同样要做品牌词
// 清洗（否则带图请求绕过了 11-128 风控缓解）。
func TestSanitizeWorkBuddyBrandWords_ArrayText(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"Anthropic's policy"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`
	out := PrepareWorkBuddyChatPayload([]byte(body))
	_, content := findImageURLPart(t, out)
	var got string
	for _, rawPart := range content {
		part, _ := rawPart.(map[string]any)
		if part["type"] == "text" {
			got, _ = part["text"].(string)
		}
	}
	if strings.Contains(got, "Anthropic's") {
		t.Fatalf("brand word not scrubbed in array text block: %q", got)
	}
	if !strings.Contains(got, "Anthropic policy") {
		t.Fatalf("unexpected text after scrub: %q", got)
	}
}

// TestWorkBuddyArrayContent_NoImageUnaffected 无图请求的行为完全不变
// （字符串 content 原样、文本数组折叠）。
func TestWorkBuddyArrayContent_NoImageUnaffected(t *testing.T) {
	// 字符串 content 不动
	out := PrepareWorkBuddyChatPayload([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	if !strings.Contains(string(out), `"content":"hi"`) {
		t.Fatalf("plain string content altered: %s", out)
	}
	// 空数组折叠为空字符串
	out = PrepareWorkBuddyChatPayload([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":[]}]}`))
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	content := obj["messages"].([]any)[0].(map[string]any)["content"]
	if s, ok := content.(string); !ok || s != "" {
		t.Fatalf("empty array should fold to empty string, got %T %v", content, content)
	}
}
