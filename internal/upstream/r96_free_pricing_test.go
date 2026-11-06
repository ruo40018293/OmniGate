package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"omnigate/internal/auth"
	"omnigate/internal/provider"
)

// R96：上游对免费模型给的是**显式** "x0.00 credits" + badge:限时免费，
// 这与「压根没有 credits 字段的不计费内部模型」是两回事。
// 旧实现用 rate<=0 一刀切，把前者一起丢掉，导致：
//   - 费率表里没有这一行（面板当作「上游未返回倍率」默认隐藏）；
//   - /v1/models 少了 "(0.00x)" 后缀，用户看不出它免费；
//   - 用户只能在官网看到「限时免费」，网关侧完全不可见。
func TestFetchModelPricingKeepsExplicitFreeModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/console/enterprises/personal/models" {
			t.Errorf("unexpected path %q（CN 区应打 /console/...）", r.URL.Path)
		}
		io.WriteString(w, `{"code":0,"data":{"models":[`+
			`{"id":"hy4free","credits":"x0.00 credits","tags":["craft","badge:限时免费:#FF0000"]},`+
			`{"id":"hy3","credits":"x0.00 credits","tags":["craft"]},`+
			`{"id":"hy4std","credits":"x0.29 credits"},`+
			`{"id":"hunyuan-chat","credits":""},`+
			`{"id":"auto","tags":["craft"]}`+
			`]}}`)
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	rows, err := c.FetchModelPricing(&auth.Auth{})
	if err != nil {
		t.Fatalf("FetchModelPricing: %v", err)
	}
	byID := make(map[string]provider.ModelPricing, len(rows))
	for _, r := range rows {
		byID[r.Model] = r
	}

	// 免费模型必须保留，且倍率是「显式 0」而不是「未知」。
	for _, id := range []string{"hy4free", "hy3"} {
		r, ok := byID[id]
		if !ok {
			t.Fatalf("%s 被丢弃了（R96：显式 x0.00 必须保留，否则免费模型在网关侧不存在）", id)
		}
		if r.Rate != 0 {
			t.Errorf("%s rate=%v，应为 0", id, r.Rate)
		}
		if !r.IsExplicit() {
			t.Errorf("%s 必须标记为显式倍率（否则面板会显示 unknown 而不是 Free）", id)
		}
	}
	if r := byID["hy4free"]; r.Note != "限时免费" || r.Color != "#FF0000" {
		t.Errorf("hy4free 促销标签解析错：note=%q color=%q", r.Note, r.Color)
	}

	// 正常付费模型不受影响。
	if r, ok := byID["hy4std"]; !ok || r.Rate != 0.29 || !r.IsExplicit() {
		t.Errorf("hy4std 解析异常：%+v ok=%v", r, ok)
	}

	// 无 credits 字段的不计费内部模型仍要丢掉（别把「缺字段」当免费）。
	if _, ok := byID["hunyuan-chat"]; ok {
		t.Error("hunyuan-chat 无 credits 字段，应跳过，不能当成免费模型")
	}

	// auto 保留但非显式（它没有 credits，倍率 0 无意义）。
	if r, ok := byID["auto"]; !ok {
		t.Error("auto 应保留（调度器模型，仍可调用）")
	} else if r.IsExplicit() {
		t.Error("auto 无 credits 字段，必须 Explicit=false，否则面板会误显示 Free")
	}
}

// R96 的第二半：免费模型出现在费率表之后，/v1/models 的 id 必须带 (0.00x)，
// 否则客户端下拉框里它仍然是个「看不出免费」的裸名字。
func TestFreeModelGetsZeroRateSuffix(t *testing.T) {
	if got := provider.RateSuffixFormat(0, true); got != " (0.00x)" {
		t.Fatalf("RateSuffixFormat(0,true)=%q，应为 \" (0.00x)\"", got)
	}
	if got := provider.RateSuffixFormat(0, false); got != "" {
		t.Fatalf("非显式倍率不该加后缀，got %q", got)
	}
}
