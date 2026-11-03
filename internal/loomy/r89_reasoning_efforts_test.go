// r89_reasoning_efforts_test.go 锁住 R89：loomy 的 `/models` 是思考档位表的
// 权威来源，必须带进 provider.ModelInfo，/v1/models 才能告诉客户端有哪些档可选。
//
// 之前这些字段只用于出站投影 projectEffort，到 ModelInfo 就断了——
// 于是模型接受 reasoning_effort，用户却看不到档位清单。
package loomy

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"omnigate/internal/auth"
)

// TestFetchModelsDeclaresReasoningEfforts 上游声明档位就原样声明，默认档也带上。
func TestFetchModelsDeclaresReasoningEfforts(t *testing.T) {
	c := New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{
			"id":"spark-x","name":"Spark X2.5",
			"context_length":256000,"max_output_tokens":32000,
			"reasoning_efforts":["low","medium","high"],
			"default_reasoning_effort":"medium",
			"capabilities":{"reasoning":true,"vision":true}
		}]}`))
	}))
	defer srv.Close()
	c.Base = srv.URL

	got, err := c.FetchModels(&auth.Auth{AccessToken: "dummy"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应产出 1 条，得到 %d", len(got))
	}
	if !reflect.DeepEqual(got[0].ReasoningEfforts, []string{"low", "medium", "high"}) {
		t.Errorf("ReasoningEfforts 应原样透传，得到 %#v", got[0].ReasoningEfforts)
	}
	if got[0].DefaultReasoningEffort != "medium" {
		t.Errorf("DefaultReasoningEffort 应为 medium，得到 %q", got[0].DefaultReasoningEffort)
	}
}

// TestFetchModelsNoEffortsStaysEmpty 上游没声明就留空（nil/空），不许补默认三档。
func TestFetchModelsNoEffortsStaysEmpty(t *testing.T) {
	c := New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"plain","name":"Plain","context_length":128000}]}`))
	}))
	defer srv.Close()
	c.Base = srv.URL

	got, err := c.FetchModels(&auth.Auth{AccessToken: "dummy"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(got[0].ReasoningEfforts) != 0 {
		t.Errorf("上游未声明档位时不得编造，得到 %#v", got[0].ReasoningEfforts)
	}
	if got[0].DefaultReasoningEffort != "" {
		t.Errorf("上游未声明默认档时应为空，得到 %q", got[0].DefaultReasoningEffort)
	}
}

// TestFetchModelsCleansEfforts 空串/重复档位在入口处清掉，避免 /v1/models 出现脏档。
func TestFetchModelsCleansEfforts(t *testing.T) {
	c := New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{
			"id":"dirty","name":"Dirty",
			"reasoning_efforts":["  low  ","","low"," high "],
			"default_reasoning_effort":"  low  "
		}]}`))
	}))
	defer srv.Close()
	c.Base = srv.URL

	got, err := c.FetchModels(&auth.Auth{AccessToken: "dummy"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if !reflect.DeepEqual(got[0].ReasoningEfforts, []string{"low", "high"}) {
		t.Errorf("档位应去空去重并 trim，得到 %#v", got[0].ReasoningEfforts)
	}
	if got[0].DefaultReasoningEffort != "low" {
		t.Errorf("默认档应 trim，得到 %q", got[0].DefaultReasoningEffort)
	}
}
