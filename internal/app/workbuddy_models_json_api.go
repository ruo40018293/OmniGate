package app

// 一键写入 WorkBuddy 自定义模型的 HTTP 接口。
//
// ## 为什么拆成「预览 + 确认」两步
//
// 写入是**整体覆盖**用户的 models.json —— 里面有他手工加的条目、可能还有别的工具
// 写的内容。只做一步（POST 直接写）的话，覆盖发生在他看到任何提示之前；
// 只做前端 confirm 也不够：提示里需要「现有多少条会被覆盖」这种真实数字，
// 而那要后端先读一次文件。
//
// 故：
//   - GET  /api/fees/models_json → 只读预览（路径、现有条数、将写入多少条、是否覆盖）
//   - POST /api/fees/models_json → 真正写入；**必须显式 confirm=true**，
//     否则 400。不带 confirm 就打过来通常是脚本/误触，不该造成数据丢失。

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"omnigate/internal/provider"
)

// modelsJSONPreview 预览结果。
type modelsJSONPreview struct {
	Path string `json:"path"`
	// Exists 目标文件是否已存在（决定是否需要覆盖警告）。
	Exists bool `json:"exists"`
	// ExistingCount 已存在的条目数（覆盖时会丢多少条）。
	ExistingCount int `json:"existing_count"`
	// WillWrite 即将写入的条目数。
	WillWrite int `json:"will_write"`
	// Skipped 无倍率、未纳入的模型数。
	Skipped int `json:"skipped"`
	// Endpoint 与 APIKeySet 只回显「是否已设置」，绝不回显 key 值。
	Endpoint  string `json:"endpoint"`
	APIKeySet bool   `json:"api_key_set"`
}

// previewWorkBuddyModelsJSON 只读预览，不落任何盘。
func (a *App) previewWorkBuddyModelsJSON(exclude []provider.Kind) (*modelsJSONPreview, error) {
	path, err := ModelsJSONPath()
	if err != nil {
		return nil, err
	}
	pv := &modelsJSONPreview{
		Path:      path,
		Endpoint:  a.modelsJSONEndpoint(),
		APIKeySet: a.adminAPIKey() != "",
	}
	if st, serr := os.Stat(path); serr == nil && !st.IsDir() {
		pv.Exists = true
		pv.ExistingCount = countJSONArray(path)
	}

	skip := make(map[string]bool, len(exclude))
	for _, k := range exclude {
		skip[k.String()] = true
	}
	a.pricingMu.Lock()
	cached := a.pricingCache
	a.pricingMu.Unlock()
	var modelsByKind map[provider.Kind][]provider.ModelInfo
	if a.handler != nil {
		modelsByKind = a.handler.CachedChannelModels()
	}
	for _, ch := range buildFeesChannels(modelsByKind, cached, feeChannelOrder) {
		if skip[ch.Channel] {
			continue
		}
		for _, m := range ch.Models {
			if m.Priced {
				pv.WillWrite++
			} else {
				pv.Skipped++
			}
		}
	}
	return pv, nil
}

// registerModelsJSONRoutes 注册预览与写入两个端点。
func (a *App) registerModelsJSONRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/fees/models_json", func(w http.ResponseWriter, r *http.Request) {
		pv, err := a.previewWorkBuddyModelsJSON(nil)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, pv)
	})

	mux.HandleFunc("POST /api/fees/models_json", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			// Confirm 必须显式为 true 才写入：覆盖是破坏性操作，
			// 不设默认 true —— 宁可多一次点击，也不丢用户数据。
			Confirm bool     `json:"confirm"`
			Exclude []string `json:"exclude,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		if !req.Confirm {
			apiError(w, http.StatusBadRequest,
				"该操作会覆盖已有的自定义模型清单，请先在面板确认警告后再提交（confirm=true）")
			return
		}
		res, err := a.WriteWorkBuddyModelsJSON(trimModelsExclude(req.Exclude))
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("models.json 已写入：%s（%d 条，覆盖旧文件=%t，备份=%s）", res.Path, res.Count, res.Existed, res.Backup)
		writeJSON(w, http.StatusOK, res)
	})
}
