package raccoon

import "testing"

// R88：识图能力以**实测表**优先，纠正上游 catalog 的漏标与误标。
// 只信 tags 的两个方向都会错：漏标把能读图的模型声明成 text-only，误标让客户端发图必失败。
func TestMeasuredVisionOverridesCatalogTags(t *testing.T) {
	cases := []struct {
		name string
		id   string
		tags []string
		want bool
	}{
		{"误标：上游给 vision 标签但实测不支持", "sn-glm-5-3", []string{"vision"}, false},
		{"漏标：上游无标签但实测可读图", "raccoon-19b265", nil, true},
		{"漏标（空切片）", "raccoon-405a1c", []string{}, true},
		{"实测与标签一致", "sn-kimi-k3", nil, true},
		{"目录换 ID 后的真实模型名", "sn-deepseek-v4-1-flash-1017", nil, true},
		{"表外模型跟随 tags（有标签）", "some-new-model", []string{"vision"}, true},
		{"表外模型跟随 tags（无标签）", "some-new-model", []string{"reasoning"}, false},
	}
	for _, tc := range cases {
		if got := supportsImages(tc.id, tc.tags); got != tc.want {
			t.Errorf("%s: supportsImages(%q, %v) = %v，期望 %v", tc.name, tc.id, tc.tags, got, tc.want)
		}
	}
}

// 静态兜底表必须与实测表一致：目录接口不可用时，客户端只能看到这张表。
func TestStaticModelsAdvertiseMeasuredCapabilities(t *testing.T) {
	got := map[string]bool{}
	for _, m := range staticModels {
		got[m.ID] = m.SupportsImages
	}
	if !got["raccoon-19b265"] {
		t.Error("raccoon-19b265 实测可读图，静态表未声明 SupportsImages")
	}
	if !got["raccoon-405a1c"] {
		t.Error("raccoon-405a1c 实测可读图，静态表未声明 SupportsImages")
	}
	if v, ok := got["sn-glm-5-3"]; !ok {
		t.Error("静态表缺 sn-glm-5-3")
	} else if v {
		t.Error("sn-glm-5-3 实测不支持 image_url，静态表不得声明 SupportsImages")
	}
	if _, ok := got["sn-deepseek-v4-1-flash"]; ok {
		t.Error("静态表仍含旧 ID sn-deepseek-v4-1-flash（生产日志 1372 次请求全是 -1017）")
	}
	if !got["sn-deepseek-v4-1-flash-1017"] {
		t.Error("静态表缺当前真实模型 ID sn-deepseek-v4-1-flash-1017")
	}
}
