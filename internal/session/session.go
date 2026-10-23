// Package session 会话粘性路由：同一会话（conversationId / 派生内容键）尽量绑定同一账号。
//
// ## 为什么需要它（与既有「请求计数粘性」的区别）
//
// 既有粘性（internal/server 的 stickyEntry）按**请求次数**轮换：连续成功 50 次后换号。
// 但它不看会话边界，于是：
//   - 客户端开一段新对话，可能半路接到别的号上 —— 上下文里的工具调用 ID、
//     assistant 消息 offset 之类对不上，上游 prompt 缓存也直接 miss；
//   - 一个长会话被打散到多个号，既伤缓存也伤体验。
//
// 本包按**会话**绑定：同一 conversation 恒定落到同一账号，直到该号不可用才重分配。
//
// ## 设计要点
//
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先「空闲账号」（未绑定任何会话的可用号），其次全池哈希（双段策略）——
//     均匀打散的同时尽量不挤占已有会话；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性清理；
//   - **纯内存**，不引 Redis：本项目定位是单机自用，多实例持久化需求不存在
//     （原实现有 Upstash 镜像，那是给多副本部署的）。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// Config 路由依赖。
type Config struct {
	// TTL 会话绑定有效期：超过则重新分配（防止长期堆积 + 让空闲号有机会被用上）。
	TTL time.Duration
	// GCInterval 后台清理间隔。
	GCInterval time.Duration
	// Available 返回某渠道下可用账号 uid（healthy 的有序列）。
	//
	// 渠道参数存在的原因：本项目是多渠道聚合（11 个渠道各有独立账号池），
	// 而会话粘性是**全局**路由器。若可用列表不分渠道，分配时可能给 traework 的
	// 会话挑中 workbuddy 的账号 —— 那个 uid 在 traework 池里根本不存在。
	// 会话键已带渠道前缀，故 channel 参数足以定位正确的池。
	Available func(channel string) []string
	// AvailableForModel 按渠道 + 请求模型返回可用账号。nil 时回落 Available。
	//
	// 为什么粘性需要模型维度：绑定只记 uid，而同一会话可能换模型。账号被 6004
	// 模型级限额后对**其他模型**仍可用；若只按账号级可用性校验，会话会被钉在
	// 这个「对当前模型不可用」的号上反复失败 —— 正是「限额后换不动号」的观感来源。
	AvailableForModel func(channel, model string) []string
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	stop    chan struct{}
	// stopDone 在 GC goroutine 退出时关闭；StopGC 等它，保证关停是同步的。
	// 只有 close(stop) 的话，正卡在 gcOnce 里的那一轮可能还没跑完就返回 ——
	// 关停后立刻写入的过期项会被它顺手删掉（TestGCLifecycle 曾 ~1/3 概率红）。
	stopDone chan struct{}
}

// New 构建路由器。TTL/GCInterval 非正取默认（30m / 5m）；Available nil 视为空池。
func New(cfg Config) *Router {
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// StartGC 启动后台 GC（幂等）。进程退出时调 StopGC。
//
// stop channel 必须在启 goroutine 前捕获到**局部变量**：goroutine 在 select 里
// 每轮重新求值 r.stop 是无锁读，而 StopGC 持写锁把它置 nil —— 既是数据竞争，
// 又会在读到 nil 后让该 case 永久阻塞（nil channel 永不就绪），关停彻底失效：
// goroutine 永不退出，ticker 无限触发 gcOnce。捕获局部变量后 close(stop) 与
// select 观测的是同一个 channel，StopGC 一定能让 goroutine 退出。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	r.stop = stop
	r.stopDone = done
	r.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
//
// 关停必须**同步**：close(stop) 只是发信号，goroutine 可能正卡在 gcOnce 里持着
// 写锁。若 StopGC 立刻返回，调用方随后写入的过期项会被那一轮还在飞的 gcOnce
// 顺手删掉 —— TestGCLifecycle 在 2026-10-23 实测三次两红，根因正是这个窗口
// （测试注释里记的「stop channel 竞态」是 2026-08 修掉的旧 bug，不覆盖本症状）。
// 现在靠 stopDone 等 goroutine 真正退出。注意：不能在持锁时等，否则 gcOnce
// 永远拿不到锁。
func (r *Router) StopGC() {
	r.mu.Lock()
	if r.stop == nil {
		r.mu.Unlock()
		return
	}
	close(r.stop)
	done := r.stopDone
	r.stop = nil
	r.stopDone = nil
	r.mu.Unlock()
	if done != nil {
		<-done
	}
}

// Resolve 返回会话 key 在 channel 渠道下应绑定的账号 uid，ok=false 表示无可用账号。
func (r *Router) Resolve(channel, key string) (string, bool) {
	return r.ResolveForModel(channel, key, "")
}

// ResolveForModel 返回会话 key 在 channel 渠道 + 该模型下应绑定的账号 uid。
// 命中且账号可用 → 滚动 lastActive 并直接返回；否则重新分配。
//
// 调用方负责把渠道拼进 key（见 internal/session 的包注释「多渠道」说明）：
// 同一 conversationId 在不同渠道下必须能各自绑定，故键空间需隔离。
func (r *Router) ResolveForModel(channel, key, model string) (string, bool) {
	if key == "" {
		return "", false
	}
	now := time.Now()
	available := r.availableSet(channel, model)

	// ── Fast path: RLock 快查 ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) && available[e.uid] {
		r.touch(key, e.uid, now)
		return e.uid, true
	}

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好（TOCTOU 防护）。
	if e2, ok2 := r.entries[key]; ok2 && !expired(e2, now, r.cfg.TTL) {
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // 绑定号对该模型不可用：清掉再分配
	}

	uids := r.availableSlice(channel, model)
	if len(uids) == 0 {
		return "", false
	}

	// 双段策略：优先「空闲账号」（未被任何会话绑定的可用号），其次全池。
	bound := make(map[string]bool, len(r.entries))
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	idle := make([]string, 0, len(uids))
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	cands := idle
	if len(cands) == 0 {
		cands = uids
	}
	uid := cands[hashIndex(key, len(cands))]
	r.entries[key] = entry{uid: uid, lastActive: now}
	return uid, true
}

// touch 滚动 lastActive（只在快路径命中时调用）。
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值）。
//
// 供「粘性跟随最终成功号」用：请求成功返回前把会话重绑到实际成功的账号，让多轮
// 对话收敛到「对该会话持续成功」的号 —— 粘性分配可能猜错（双段哈希不保证最优），
// 真实结果才是权威。空 key/uid 直接返回。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	return found
}

// Count 当前绑定数（面板/状态观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，返回清理条数。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			delete(r.entries, key)
			n++
		}
	}
	return n
}

// availableSet 把可用列表转集合（快路径命中校验用）。
func (r *Router) availableSet(channel, model string) map[string]bool {
	uids := r.availableSlice(channel, model)
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice 安全调用 AvailableForModel；未注入时回落 Available。
func (r *Router) availableSlice(channel, model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(channel, model)
	}
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available(channel)
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（双段分配的稳定散列）。
//
// 不用 map 迭代顺序或随机数：同一 key 必须恒定映射到同一账号，
// 否则重启后全部粘性丢失。
func hashIndex(key string, n int) int {
	if n <= 0 {
		return 0
	}
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// derivedKeyPrefix 派生键前缀，与显式会话 id 的命名空间隔离：
// 即便客户端恰好传了形如 "d-<hex>" 的显式 id 也不至于与派生键混淆。
const derivedKeyPrefix = "d-"

// ExtractKey 从请求体提取会话键；按下列顺序依次尝试，找不到时回退到内容派生的
// 稳定键（见 deriveKey），仍为空则返回空串（**绝不失败** —— 无会话键就退普通轮换）。
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. prompt_cache_key（OpenAI 前缀缓存键）
//  6. 派生键：system 提示词 + 首条用户消息的哈希
//
// ## 为什么 camelCase 与 snake_case 都要认
//
// 客户端实际发的是 camelCase 的 conversationId。只认 snake_case 会导致粘性**永不命中**
// （表现为「同一对话轮转不同账号」+ 上游上下文缓存 miss）。两者都认后，
// 同值不同名也命中同一对话，天然不混用。
//
// ## 为什么不拿 user_id 当粘性键
//
// user 维度粒度过粗：一个 user 的全部并行对话会被钉到同一账号，
// 粘性范围远大于上游 prompt cache 的对话级边界。故 user 维度**既不作键、
// 也抑制派生回退**（见 hasUserIDKey），这类客户端回落普通轮换。
//
// ## prompt_cache_key 为什么排在这里
//
// pi-ai 一类客户端（dsh 等）把会话 ID 放在这个 OpenAI 前缀缓存字段里。
// 纳入识别后无需改配置即可命中粘性；但它排在显式会话键之后，绝不抢占 conversation 维度。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	// 派生回退闸：带 user 维度标识的请求不派生（理由见上）。
	if hasUserIDKey(obj) {
		return ""
	}
	return deriveKey(obj)
}

// hasUserIDKey 报告 body 是否携带 user 维度标识（metadata.user_id 或顶层 user_id）。
func hasUserIDKey(obj map[string]any) bool {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if strOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}
	return strOrEmpty(obj["user_id"]) != ""
}

// deriveKey 从消息内容派生稳定会话键：SHA-256(system + 首条 user) 前 16 字节。
//
// ## 为什么用「system + 首条 user」而不是全部消息
//
// 多轮对话里历史消息每轮追加，全量哈希会每轮变化 → **粘性完全失效**。
// system 与首条 user 在一次对话中恒定，足以区分不同对话。
//
// ## 为什么首条 user 用内容签名而非纯文本
//
// 纯图片轮（无 text part）在纯文本提取下恒空串 → 首图会话是粘性盲区。
// 签名口径下图片 part 以 `[type]` 占位入键，既能派生又不引入不稳定内容。
func deriveKey(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	systemText, firstUser := "", ""
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch strOrEmpty(msg["role"]) {
		case "system", "developer":
			if systemText == "" {
				systemText = messageText(msg["content"])
			}
		case "user":
			if firstUser == "" {
				firstUser = userContentSignature(msg["content"])
			}
		}
		if firstUser != "" && systemText != "" {
			break // 都拿到了：停止遍历长历史
		}
	}
	if firstUser == "" {
		return "" // 无用户消息：无从归属会话
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUser))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// userContentSignature 把 content（any）转成**会话级稳定**签名，供 deriveKey 专用。
//
// 文本 part 照常拼接；非文本 part 只入 "[type]" 占位、**不入内容摘要** ——
// 同一张图的 URL/base64 会逐轮变化，内容摘要入键会让派生键随轮漂移，粘性名存实亡。
// 占位仍能区分「有无图片/图片数量」，纯图片首条消息由此可派生非空键。
func userContentSignature(content any) string {
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	st := strings.TrimSpace(string(raw))
	if st == "" || st == "null" {
		return ""
	}
	if st[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return s
	}
	if st[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	hasNonText := false
	for _, pr := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) != nil {
			return ""
		}
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
			continue
		}
		hasNonText = true
		b.WriteString("\n[" + p.Type + "]\n")
	}
	out := b.String()
	if !hasNonText {
		return out
	}
	return strings.TrimSpace(out)
}

// messageText 提取消息 content 的文本表示。
// 兼容字符串 / [{type:"text",text:"..."}] 数组（OpenAI 多模态）。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if p, ok := part.(map[string]any); ok {
				sb.WriteString(strOrEmpty(p["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// strOrEmpty 把 JSON 字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
