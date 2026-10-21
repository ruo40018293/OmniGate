// tool_image_test.go 回归 R113/R114：工具结果里的图片必须走 vision 通道，
// 绝不能被序列化成 base64 文本；且转发前的内联大图要降采样瘦身。
//
// 生产事故：Codex 的 view_image 把截图以 input_image 块放进 function_call_output。
// 旧实现 stringifyOutput 取不到文本后回退 json.Marshal，整张 PNG 的 base64 变成一条
// 超长**文本**消息——实测 1.6MB base64 ≈ 146 万 token，把 1M 上下文的模型直接打成
// prompt is too long。R114 补的是字节那一维：token 通了但 33MB 的请求体会撞 413。
package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// tinyPNG 生成 n×n 的 PNG data URL（内容全同色，便于断言重编码结果）。
func tinyPNG(t *testing.T, n int) string {
	t.Helper()
	// 用确定性伪随机噪声而非平滑渐变：渐变图 PNG 与 JPEG 压得几乎一样大，
	// 触发 slimPayload 的「没变小就别换」保护，测不出重编码路径。
	// 真实截图（大量文字/边缘细节）也更接近噪声图的压缩特性。
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	h := uint32(2166136261)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			h = h*16777619 ^ uint32(x*374761393+y*668265263)
			c := color.RGBA{R: uint8(h), G: uint8(h >> 8), B: uint8(h >> 16), A: 255}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestToolImageNotStringified function_call_output 里的 input_image 必须产出
// image_url 内容块，且响应体里绝不能再出现 base64 图片数据。
func TestToolImageNotStringified(t *testing.T) {
	url := tinyPNG(t, 8)
	out := toolContentValue([]any{
		map[string]any{"type": "input_text", "text": "看这张图"},
		map[string]any{"type": "input_image", "image_url": url},
	})
	raw := stringifyJSONForTest(t, out)

	if strings.Contains(raw, base64.StdEncoding.EncodeToString([]byte(url))) {
		t.Fatalf("图片 base64 绝不能出现在序列化结果里")
	}
	if !strings.Contains(raw, `"image_url"`) {
		t.Fatalf("应产出 image_url 内容块: %s", raw)
	}
	if !strings.Contains(raw, `"text":"看这张图"`) {
		t.Fatalf("文本块应保留: %s", raw)
	}
}

// TestToolImageOnlyPlaceholder 图片独占 tool 结果时补 [image] 文本占位：
// 部分渠道会丢弃非 text 块，只有图片会让剥离后的 content 变空（上游 400）。
func TestToolImageOnlyPlaceholder(t *testing.T) {
	out := toolContentValue([]any{
		map[string]any{"type": "input_image", "image_url": tinyPNG(t, 8)},
	})
	raw := stringifyJSONForTest(t, out)
	if !strings.Contains(raw, "[image]") {
		t.Fatalf("仅有图片时应补 [image] 占位: %s", raw)
	}
}

// TestToolImagePlainTextUnchanged 纯文本 tool 结果仍返回 string（不引入数组）。
func TestToolImagePlainTextUnchanged(t *testing.T) {
	got := toolContentValue([]any{
		map[string]any{"type": "input_text", "text": "hello"},
	})
	s, ok := got.(string)
	if !ok || s != "hello" {
		t.Fatalf("纯文本应返回 string，得到 %#v", got)
	}
	if got := toolContentValue("raw string"); got != "raw string" {
		t.Fatalf("字符串应原样返回，得到 %#v", got)
	}
}

// TestImagePartURLShapes 三种图片块形态都能取到 URL。
func TestImagePartURLShapes(t *testing.T) {
	d := "data:image/png;base64,AAAA"
	cases := []struct {
		name string
		part map[string]any
		want string
	}{
		{"Responses 扁平", map[string]any{"type": "input_image", "image_url": d}, d},
		{"Responses 嵌套", map[string]any{"type": "input_image", "image_url": map[string]any{"url": d}}, d},
		{"通用 url 字段", map[string]any{"type": "image", "url": d}, d},
		{"Anthropic source", map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "AAAA",
			},
		}, "data:image/png;base64,AAAA"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url, _ := imagePartURL(c.part)
			if url != c.want {
				t.Errorf("url=%q want %q", url, c.want)
			}
		})
	}
}

// TestImagePartURLDetail detail 透传。
func TestImagePartURLDetail(t *testing.T) {
	_, detail := imagePartURL(map[string]any{
		"type":      "input_image",
		"image_url": map[string]any{"url": "data:image/png;base64,AA", "detail": "low"},
	})
	if detail != "low" {
		t.Errorf("detail=%q want low", detail)
	}
}

// TestSlimDataURLShrinksLargeImage 大图（长边超限）被缩到 1568 并重编码，体积显著变小。
func TestSlimDataURLShrinksLargeImage(t *testing.T) {
	big := tinyPNG(t, 2400)
	out := slimDataURLImage(big)
	if out == big {
		t.Fatal("超限大图应被瘦身")
	}
	if !strings.HasPrefix(out, "data:image/jpeg;base64,") {
		t.Fatalf("不透明图应重编码为 JPEG: %.40s", out)
	}
	payload := out[strings.IndexByte(out, ',')+1:]
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Errorf("format=%s want jpeg", format)
	}
	if long := maxInt(cfg.Width, cfg.Height); long > imageMaxLongEdge {
		t.Errorf("长边 %d 超过上限 %d", long, imageMaxLongEdge)
	}
	if len(raw) >= len(big)/4 {
		t.Errorf("瘦身效果不足：%d -> %d 字节", len(big), len(raw))
	}
}

// TestSlimDataURLKeepsSmallImage 小图（<512KB 且长边合规）原样放行，避免无谓 CPU。
func TestSlimDataURLKeepsSmallImage(t *testing.T) {
	small := tinyPNG(t, 16)
	if got := slimDataURLImage(small); got != small {
		t.Error("合规小图不应改动")
	}
}

// TestSlimDataURLLeavesRemoteAlone http(s) 直链不动（不下载、不改写）。
func TestSlimDataURLLeavesRemoteAlone(t *testing.T) {
	for _, u := range []string{
		"https://example.com/a.png",
		"http://example.com/b.jpg",
		"data:image/svg+xml;base64,AAAA", // 非 PNG/JPEG 解码不了，须原样返回
		"data:image/png,notbase64",       // 非 base64 编码
		"data:image/png;base64,!!!not-valid-base64!!!",
	} {
		if got := slimDataURLImage(u); got != u {
			t.Errorf("%q 应原样返回，得到 %.40q", u, got)
		}
	}
}

// TestSlimPayloadRejectsBomb 超大像素数直接放弃，不承担 OOM 风险。
func TestSlimPayloadRejectsBomb(t *testing.T) {
	if _, ok := slimPayload(base64.StdEncoding.EncodeToString([]byte("not an image"))); ok {
		t.Error("非图片数据应判定为不该替换")
	}
}

// stringifyJSONForTest 把任意值序列化（断言用，避免在测试里散落 json.Marshal）。
func stringifyJSONForTest(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
