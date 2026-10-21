// growth_tasks.go 成长任务**编排层**：把「任务 → 一组行为」的动作表跑起来，
// 并在进度达标时自动领奖。
//
// ## 与其它文件的分工
//
//	growth.go        协议层   —— 一个函数对应一个上游端点
//	growth_auto.go   编排层   —— 连登兑换+抽奖、猫猫旅行（真实服务端动作）
//	desktop.go       —— 桌面/web 指纹与事件链构造
//	mp_report.go     —— 小程序指纹与事件构造
//	growth_tasks.go  编排层   —— 本文件：任务动作表 + 执行 + 领奖
//
// ## ⚠️ 风险与使用前提
//
// 这些动作的本质是**伪造客户端行为事件**，会真实改变账号状态（点亮进度、领取奖励）。
// 因此：
//
//  1. **默认关闭**，必须由用户在面板上显式触发（见 config.Schedule.GrowthTasks）
//  2. 字段名写错时上游**仍返回 200**（只是不入账）→ 判定一律以**回读进度**为准，
//     不能只看 HTTP 状态码。这是本文件所有 settle 逻辑存在的原因。
//  3. 事件形状来自第三方逆向成果，**本项目未独立实测**。首次使用请先对**一个**
//     账号手动执行，逐步核对哪些任务真的点亮了。
//  4. Expert_Philanthropy 需要真实捐款回执，服务端会校验 —— **无法自动化**，
//     因此 18 个任务里 17 个是上限。
//
// ## 执行顺序
//
// 先批量 accept（规范状态机；accept 不产生进度，可安全重放），
// 再按动作表逐项执行行为链路，最后回读 + 领奖。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"

	"omnigate/internal/auth"
)

// 节流。声明为包级 var 而非 const —— 测试需要把它压到毫秒级，
// 否则一个断言要等 45 秒（mp 任务的反作弊间隔）。
var (
	// growthTaskGap 普通写动作之间的间隔（对齐上游脚本实测的 1.05s 风控口径）。
	growthTaskGap = 1050 * time.Millisecond
	// growthTaskMPActionGap mp 任务的 accept / 上报 / 领奖之间的间隔。
	growthTaskMPActionGap = 2 * time.Second
	// growthTaskMPChatGap mp 对话事件的间隔。
	//
	// **不要调小**：Sequential_Tasks_3 对连发有反作弊 —— 数秒级连发会先计入进度，
	// 随后被整体回滚（claim 400 task not completed）。第三方实测 2s 连发全灭、
	// 45s 间隔全存活。
	growthTaskMPChatGap = 45 * time.Second
)

// 桌面端固定载荷（与 desktop.go 的事件链配套）。
const (
	// libraryIntroDocURL Library_read 判据事件所指页面。
	libraryIntroDocURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	// playbookCaseID / Name playbook_prompt 判据案例。
	playbookCaseID   = "pm-gtm-launch-plan"
	playbookCaseName = "新产品上市 GTM 发布计划一页纸"
	// expertLighthouseID 腾讯轻量云专家（Expert_lighthouse 判据）。
	expertLighthouseID   = "ex_2cvvUZQhDyeJ"
	expertLighthouseName = "腾讯轻量云专家"
	// nightChatModel 夜猫子任务用的模型。
	nightChatModelID   = "glm-5.2"
	nightChatModelName = "GLM-5.2"
)

// growthTaskTemplates template_5 的 5 个模板（id, name）。
var growthTaskTemplates = [][2]string{
	{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动评审"}, {"5", "代码评审"},
}

// GrowthTaskOptions 一次任务执行的选项。
type GrowthTaskOptions struct {
	// Only 限定只执行这些 task_code（空 = 全部可自动化的任务）。
	Only []string
	// IncludeMP 是否执行小程序口径任务。
	//
	// 默认 false：mp 任务因反作弊要求 45s/条 的节奏，一项就可能跑几分钟，
	// 不适合放在「一键完成」的默认路径上。用户显式要求时才纳入。
	IncludeMP bool
	// DryRun 只回读进度并汇报「打算做什么」，不发任何写请求。
	DryRun bool
	// Progress 可选的进度回调（每完成一项调用一次，供面板显示进度）。
	Progress func(done, total int, code string)
}

// GrowthTaskOutcome 单个任务的执行结果。
type GrowthTaskOutcome struct {
	TaskCode string `json:"task_code"`
	Title    string `json:"title,omitempty"`
	// Status: done | claimed | skipped | manual | error | unsupported
	Status string `json:"status"`
	Msg    string `json:"msg"`
	// Before / After 进度文本（如 "0/5"），用于让人核对是否真的动了。
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	// Credit / Energy 本项实际到账奖励（未领到为 0）。
	Credit int64 `json:"credit,omitempty"`
	Energy int64 `json:"energy,omitempty"`
}

// GrowthTaskReport 一次执行的完整报告（单个账号）。
type GrowthTaskReport struct {
	UID      string              `json:"uid"`
	Nickname string              `json:"nickname,omitempty"`
	Tasks    []GrowthTaskOutcome `json:"tasks"`
	// Credit / Energy 本次合计到账。
	Credit int64 `json:"credit"`
	Energy int64 `json:"energy"`
	// Manual 无法自动化的任务码（需在客户端手动完成）。
	Manual []string `json:"manual,omitempty"`
	// Msg 一句话总结。
	Msg string `json:"msg,omitempty"`
	// Err 整体性失败（拉取任务列表就失败等）；单项失败记在各自 Outcome 上。
	Err string `json:"err,omitempty"`
}

// ---------------------------------------------------------------------------
// 执行器
// ---------------------------------------------------------------------------

// growthTaskRunner 单账号一次执行的状态容器。
type growthTaskRunner struct {
	c    *Client
	a    *auth.Auth
	opts GrowthTaskOptions
}

// sleep 按节流间隔等待（DryRun 下不等待）。
func (r *growthTaskRunner) sleep(d time.Duration) {
	if r.opts.DryRun || d <= 0 {
		return
	}
	time.Sleep(d)
}

// GrowthRunTasks 对**单个账号**执行成长任务动作表。
//
// 返回的报告即使发生单项错误也会完整返回（Err 仅为整体性失败），
// 便于面板逐项展示「哪些成了、哪些没成」。
func (c *Client) GrowthRunTasks(a *auth.Auth, opts GrowthTaskOptions) GrowthTaskReport {
	rep := GrowthTaskReport{UID: a.UID, Nickname: a.Nickname}
	if a == nil {
		rep.Err = "账号为空"
		return rep
	}

	tasks, err := c.growthTaskList(a, opts.IncludeMP)
	if err != nil {
		rep.Err = fmt.Sprintf("拉取任务列表失败: %v", err)
		return rep
	}
	r := &growthTaskRunner{c: c, a: a, opts: opts}

	// 阶段 0：批量 accept（只对尚未接受且未领取的）。
	if !opts.DryRun {
		if msg := r.acceptPending(tasks, opts.IncludeMP); msg != "" {
			rep.Tasks = append(rep.Tasks, GrowthTaskOutcome{
				TaskCode: "(批量接受)", Status: "done", Msg: msg,
			})
		}
	}

	// 阶段 1：逐项执行。
	type planned struct {
		t   Task
		act *growthTaskAction
	}
	var plans []planned
	for _, t := range tasks {
		if len(opts.Only) > 0 && !containsCode(opts.Only, t.TaskCode) {
			continue
		}
		// 已领取的任务一律不进报告：一轮下来十几项早已领完，
		// 全列出来只会把真正失败的那几行淹没。
		if t.Claimed {
			continue
		}
		act := actionFor(t.TaskCode)
		if act == nil {
			// 无可执行动作：达标未领的直接领，其余记为「需手动」。
			if t.Claimable {
				plans = append(plans, planned{t: t})
			} else if !t.Locked {
				rep.Manual = append(rep.Manual, t.TaskCode)
			}
			continue
		}
		plans = append(plans, planned{t: t, act: act})
	}

	total := len(plans)
	for i, p := range plans {
		out := GrowthTaskOutcome{
			TaskCode: p.t.TaskCode, Title: taskTitle(p.t),
			Before: progressText(p.t),
		}
		switch {
		case p.t.Claimed:
			out.Status = "skipped"
			out.Msg = "已领取"
		case p.act == nil:
			// 达标未领：直接领。
			out.Status = "claimed"
			out.Msg = "进度已达标，尝试领奖"
			out.Credit, out.Energy, out.Msg = r.claim(p.t, out.Msg)
		default:
			out = r.runOne(p.t, *p.act, out)
		}
		rep.Credit += out.Credit
		rep.Energy += out.Energy
		rep.Tasks = append(rep.Tasks, out)
		if opts.Progress != nil {
			opts.Progress(i+1, total, p.t.TaskCode)
		}
		r.sleep(growthTaskGap) // 项间节流
	}

	rep.Msg = summarize(rep)
	return rep
}

// growthTaskList 拉取任务列表；includeMP 时并入小程序口径列表（超集，需去重）。
func (c *Client) growthTaskList(a *auth.Auth, includeMP bool) ([]Task, error) {
	tasks, err := c.ListTasks(a)
	if err != nil {
		return nil, err
	}
	if !includeMP {
		return tasks, nil
	}
	mp, err := c.ListTasksMP(a)
	if err != nil {
		// mp 列表失败不致命：默认口径的任务仍可执行。
		return tasks, nil
	}
	seen := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		seen[t.TaskCode] = true
	}
	for _, t := range mp {
		if !seen[t.TaskCode] {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

// acceptPending 批量接受尚未接受的任务（accept 不产生进度，可安全重放）。
func (r *growthTaskRunner) acceptPending(tasks []Task, includeMP bool) string {
	var normal, mp []string
	for _, t := range tasks {
		if t.Claimed || t.Locked {
			continue
		}
		if t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if isMPTaskCode(t.TaskCode) {
			mp = append(mp, t.TaskCode)
			continue
		}
		normal = append(normal, t.TaskCode)
	}
	var parts []string
	if len(normal) > 0 {
		if err := r.c.AcceptTasks(r.a, normal); err != nil {
			parts = append(parts, fmt.Sprintf("接受 %d 个失败（不阻塞）", len(normal)))
		} else {
			parts = append(parts, fmt.Sprintf("已接受 %d 个", len(normal)))
		}
	}
	if includeMP && len(mp) > 0 {
		if err := r.c.AcceptTasksMP(r.a, mp); err != nil {
			parts = append(parts, fmt.Sprintf("接受 %d 个小程序任务失败（不阻塞）", len(mp)))
		} else {
			parts = append(parts, fmt.Sprintf("已接受 %d 个小程序任务", len(mp)))
		}
	}
	return strings.Join(parts, "；")
}

// runOne 执行单个任务的动作并回读/领奖。
func (r *growthTaskRunner) runOne(t Task, act growthTaskAction, out GrowthTaskOutcome) GrowthTaskOutcome {
	if r.opts.DryRun {
		out.Status = "skipped"
		out.Msg = "试运行：" + act.Desc
		return out
	}
	if msg, err := act.Run(r); err != nil {
		if IsGrowthUnsupported(err) {
			out.Status = "unsupported"
			out.Msg = "渠道不支持该任务：" + err.Error()
			return out
		}
		out.Status = "error"
		out.Msg = err.Error()
		return out
	} else {
		out.Status = "done"
		out.Msg = msg
	}
	// 回读并领奖：**判定唯一依据**（事件上报返回 200 不代表入账）。
	after := r.readTask(t.TaskCode)
	if after != nil {
		out.After = progressText(*after)
	}
	if after != nil && (after.Claimable || (after.Target > 0 && after.Current >= after.Target && !after.Claimed)) {
		credit, energy, cmsg := r.claimRaw(t.TaskCode)
		out.Credit, out.Energy = credit, energy
		if cmsg != "" {
			out.Msg += "；" + cmsg
		}
		if credit > 0 || energy > 0 {
			out.Status = "claimed"
		}
	} else if after != nil && !after.Claimed && after.Target > 0 && after.Current < after.Target {
		out.Msg += fmt.Sprintf("；进度 %s（未达 %d，可能字段未对上或需真人节奏）",
			progressText(*after), after.Target)
	}
	return out
}

// claim 领奖并返回 (credit, energy, 人类可读结果)。
func (r *growthTaskRunner) claim(t Task, prefix string) (int64, int64, string) {
	if r.opts.DryRun {
		return 0, 0, prefix + "（试运行，未执行）"
	}
	credit, energy, msg := r.claimRaw(t.TaskCode)
	if msg != "" {
		return credit, energy, prefix + "；" + msg
	}
	if credit > 0 || energy > 0 {
		return credit, energy, fmt.Sprintf("%s；已领奖 +%d 分 +%d 能", prefix, credit, energy)
	}
	return credit, energy, prefix + "；奖励此前已领取"
}

// claimRaw 按任务口径选择领奖端点。
func (r *growthTaskRunner) claimRaw(code string) (int64, int64, string) {
	var res ClaimResult
	var err error
	if isMPTaskCode(code) {
		res, err = r.c.ClaimRewardMP(r.a, code)
	} else {
		res, err = r.c.ClaimReward(r.a, code)
	}
	if err != nil {
		return 0, 0, "领奖失败：" + err.Error()
	}
	if res.AlreadyClaimed {
		return 0, 0, ""
	}
	return res.Credit, res.Energy, ""
}

// readTask 回读单个任务的进度（默认口径查不到时回落小程序口径）。
func (r *growthTaskRunner) readTask(code string) *Task {
	if t, err := r.c.taskByCodeDefault(r.a, code); err == nil && t != nil {
		return t
	}
	if tasks, err := r.c.ListTasksMP(r.a); err == nil {
		for i := range tasks {
			if tasks[i].TaskCode == code {
				return &tasks[i]
			}
		}
	}
	return nil
}

// taskByCodeDefault 在默认口径任务列表中定位单个任务；未找到返回 (nil, nil)。
func (c *Client) taskByCodeDefault(a *auth.Auth, code string) (*Task, error) {
	tasks, err := c.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// 动作表
// ---------------------------------------------------------------------------

// growthTaskAction 一个可执行的任务动作。
type growthTaskAction struct {
	Code string
	Desc string
	Run  func(r *growthTaskRunner) (string, error)
}

// modelChatAction 前缀动作：Model_chat_*（体验指定模型对话一次）。
// 用单独的变量而不是表项，因为它一次覆盖一族 task_code。
var modelChatAction = growthTaskAction{
	Code: "Model_chat_*",
	Desc: "接受任务 → 与指定模型真实对话一次 → 对齐模型上报",
	Run:  (*growthTaskRunner).runModelChat,
}

// growthTaskActions 任务动作表。顺序即执行顺序：
// 依赖项在前（first_buddy 依赖活跃上报解锁，chat_5 也自带上报）。
var growthTaskActions = []growthTaskAction{
	{Code: "chat_5", Desc: "上报对话活跃事件补齐差额", Run: (*growthTaskRunner).runChat5},
	{Code: "first_buddy", Desc: "上报解锁 → 同意协议 → 领取第一只 Buddy", Run: (*growthTaskRunner).runFirstBuddy},
	{Code: "RichMeow_Chat", Desc: "桌面端成功对话事件链", Run: (*growthTaskRunner).runRichMeow},
	{Code: "Buddy_App", Desc: "进入 Buddy 应用事件链", Run: (*growthTaskRunner).runBuddyApp},
	{Code: "Buddy_App_QQ", Desc: "进入企鹅教师助手事件链", Run: (*growthTaskRunner).runBuddyApp},
	{Code: "automation_1", Desc: "上报定时任务创建事件", Run: (*growthTaskRunner).runAutomationCreate},
	{Code: "Library_read", Desc: "上报资料库介绍阅读事件（Web 域）", Run: (*growthTaskRunner).runLibraryRead},
	{Code: "template_5", Desc: "上报模板使用事件组 ×N", Run: (*growthTaskRunner).runTemplateUse},
	{Code: "playbook_prompt", Desc: "上报灵感案例发送 Prompt 事件组", Run: (*growthTaskRunner).runPlaybookPrompt},
	{Code: "create_canvas", Desc: "上报设计创意画布事件组", Run: (*growthTaskRunner).runCreateCanvas},
	{Code: "Hp_Appearance", Desc: "设置主题 + 上报皮肤生效事件", Run: (*growthTaskRunner).runAppearance},
	{Code: "skill_1", Desc: "真实对话 + skill_info 技能加载事件", Run: (*growthTaskRunner).runSkillFresh},
	{Code: "expert_5", Desc: "真实召唤并使用 5 个平台专家", Run: (*growthTaskRunner).runExpertBatch5},
	{Code: "Expert_team_use_3", Desc: "真实召唤并使用 3 个专家团", Run: (*growthTaskRunner).runExpertTeam3},
	{Code: "Expert_lighthouse", Desc: "真实召唤轻量云专家并使用（LOCAL 口径）", Run: (*growthTaskRunner).runExpertLighthouse},
	{Code: "black_cat", Desc: "夜间窗口内补足对话次数", Run: (*growthTaskRunner).runBlackCat},

	// ---- 小程序口径（默认不跑，见 GrowthTaskOptions.IncludeMP）----
	{Code: "school_season", Desc: "小程序对话事件（带校园日 activityId）", Run: (*growthTaskRunner).runSchoolSeason},
	{Code: "Sequential_Tasks_1", Desc: "小程序内完成 1 次对话", Run: (*growthTaskRunner).runMPChat1},
	{Code: "Sequential_Tasks_2", Desc: "小程序内使用市场专家", Run: (*growthTaskRunner).runMPExpert},
	{Code: "Sequential_Tasks_3", Desc: "小程序内完成 5 次对话（真人节奏）", Run: (*growthTaskRunner).runMPChatN},
	{Code: "Sequential_Tasks_6", Desc: "小程序内完成 10 次对话（真人节奏）", Run: (*growthTaskRunner).runMPChatN},
	{Code: "Sequential_Tasks_4", Desc: "小程序定时任务（判据疑为桌面口径）", Run: (*growthTaskRunner).runMPAutomation},
	{Code: "Sequential_Tasks_5", Desc: "小程序内使用 GLM-5.2", Run: (*growthTaskRunner).runMPModelChat},
	{Code: "Sequential_Tasks_7", Desc: "小程序内体验灵感功能", Run: (*growthTaskRunner).runMPPlaybook},
}

// mpTaskCodes 小程序口径专属任务码（决定查询/接受/领奖走 mp 变体）。
var mpTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// isMPTaskCode 报告任务是否小程序口径专属。
func isMPTaskCode(code string) bool { return mpTaskCodes[strings.TrimSpace(code)] }

// actionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func actionFor(code string) *growthTaskAction {
	code = strings.TrimSpace(code)
	if strings.HasPrefix(code, "Model_chat_") {
		return &modelChatAction
	}
	for i := range growthTaskActions {
		if growthTaskActions[i].Code == code {
			return &growthTaskActions[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 各动作实现
// ---------------------------------------------------------------------------

// needFor 计算该任务还差多少次（target 非正时用 fallback）。
func needFor(t Task, fallback int64) int64 {
	target := t.Target
	if target <= 0 {
		target = fallback
	}
	if n := target - t.Current; n > 0 {
		return n
	}
	return 0
}

// runChat5 上报 chat_request_send 补齐 chat_5 的差额。
func (r *growthTaskRunner) runChat5() (string, error) {
	return r.reportChatTimes("chat_5", 5, "", "")
}

// reportChatTimes 按差额上报 N 条 chat_request_send（走 billing 域，1.05s 间隔）。
// modelID 非空时对齐模型字段（Model_chat_* 与黑猫任务用）。
func (r *growthTaskRunner) reportChatTimes(code string, fallback int64, modelID, modelName string) (string, error) {
	t, err := r.c.taskByCodeDefault(r.a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "该账号无此任务", nil
	}
	need := needFor(*t, fallback)
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-%s-%d-%d", strings.ToLower(code), time.Now().UnixMilli(), i)
		var err error
		if modelID != "" {
			err = r.c.ReportEvents(r.a, []map[string]any{
				ChatRequestSendEventModel(r.a, cid, "", modelID, modelName),
			})
		} else {
			err = r.c.ReportChatActivity(r.a, cid, "")
		}
		if err != nil {
			return fmt.Sprintf("上报第 %d/%d 条失败", i+1, need), err
		}
		if i < need-1 {
			r.sleep(growthTaskGap)
		}
	}
	return fmt.Sprintf("已上报 %d 条对话事件", need), nil
}

// runModelChat 处理 Model_chat_*：accept → 真实对话 → 对齐模型上报。
func (r *growthTaskRunner) runModelChat() (string, error) {
	// 这里拿不到 task_code（动作表用前缀匹配），从任务列表现查一次以确定 code。
	code, modelID, modelName, err := r.resolveModelChatCode()
	if err != nil {
		return "", err
	}
	if code == "" {
		return "未找到可执行的 Model_chat_* 任务", nil
	}
	if err := r.c.AcceptTasks(r.a, []string{code}); err != nil {
		// accept 失败不阻塞：行为事件才是判据。
		_ = err
	}
	r.sleep(growthTaskGap)
	if err := r.chatOnce(modelID); err != nil {
		return "", fmt.Errorf("对话请求失败: %w", err)
	}
	r.sleep(growthTaskGap)
	return r.reportChatTimes(code, 1, modelID, modelName)
}

// resolveModelChatCode 在任务列表里找一个尚未完成的 Model_chat_*，并解析出模型 id。
func (r *growthTaskRunner) resolveModelChatCode() (code, modelID, modelName string, err error) {
	tasks, lerr := r.c.ListTasks(r.a)
	if lerr != nil {
		return "", "", "", lerr
	}
	for _, t := range tasks {
		if !strings.HasPrefix(t.TaskCode, "Model_chat_") || t.Claimed {
			continue
		}
		if t.Target > 0 && t.Current >= t.Target {
			continue
		}
		id, name, ok := modelFromTaskCode(t.TaskCode)
		if !ok {
			continue
		}
		return t.TaskCode, id, name, nil
	}
	return "", "", "", nil
}

// modelFromTaskCode 由 task_code 推导模型 id / 显示名。
//
// 只对已确认的少数几个做映射；推导不出来时返回 false（宁可不动，
// 也不要拿一个猜的模型名去请求 —— 那既浪费次数又可能触发风控）。
func modelFromTaskCode(code string) (id, name string, ok bool) {
	suffix := strings.TrimPrefix(code, "Model_chat_")
	switch strings.ToUpper(suffix) {
	case "GLM5.2", "GLM-5.2":
		return "glm-5.2", "GLM-5.2", true
	case "DEEPSEEK", "DEEPSEEK-V4":
		return "deepseek-v4-flash", "DeepSeek V4 Flash", true
	default:
		return "", "", false
	}
}

// chatOnce 发一次真实对话（读干 SSE，避免残留连接）。
func (r *growthTaskRunner) chatOnce(modelID string) error {
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": "hi，请回复一句话"},
		},
		"stream": true,
	})
	rc, status, respBody, err := r.c.ChatStream(r.a, body)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("http %d: %s", status, truncate(string(respBody), 160))
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	return nil
}

// runFirstBuddy 领养：report（解锁前置）→ agreement → first。
func (r *growthTaskRunner) runFirstBuddy() (string, error) {
	if err := r.c.ReportChatActivity(r.a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("前置上报失败: %w", err)
	}
	r.sleep(growthTaskGap)
	if err := r.c.BuddyAgreement(r.a); err != nil {
		return "", fmt.Errorf("同意协议失败: %w", err)
	}
	if err := r.c.BuddyFirstAdopt(r.a); err != nil {
		if IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），稍后自动重试", nil
		}
		return "", fmt.Errorf("领取 Buddy 失败: %w", err)
	}
	return "已领取 Buddy", nil
}

// runRichMeow 桌面端成功对话事件链（RichMeow_Chat）。
func (r *growthTaskRunner) runRichMeow() (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-rm-%d", ms)
	req := fmt.Sprintf("wb2api-rm-req-%d", ms)
	events := DesktopChatSequence(conv, req, fmt.Sprintf("req-%d-user", ms), "fast-model", "fast-model")
	if err := r.c.ReportDesktopEvent(r.a, events...); err != nil {
		return "", err
	}
	return "已按桌面指纹上报成功对话事件链", nil
}

// runBuddyApp 进入 Buddy 应用事件链（同时覆盖 Buddy_App 与 Buddy_App_QQ）。
func (r *growthTaskRunner) runBuddyApp() (string, error) {
	events := DesktopBuddyAppSequence(DesktopBuddyAppQQID, DesktopBuddyAppQQName)
	if err := r.c.ReportDesktopEvent(r.a, events...); err != nil {
		return "", err
	}
	return "已上报进入 Buddy 应用事件链", nil
}

// runAutomationCreate 定时任务创建事件（automation_1）。
func (r *growthTaskRunner) runAutomationCreate() (string, error) {
	if err := r.c.ReportDesktopEvent(r.a, DesktopAutomationCreateEvent("omnigate 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead 资料库介绍阅读事件（Web 域）。
func (r *growthTaskRunner) runLibraryRead() (string, error) {
	if err := r.c.ReportWebEvent(r.a, "web_element_click", libraryIntroDocURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

// runTemplateUse 模板使用事件组 ×N（N 取任务 target，缺省 5）。
func (r *growthTaskRunner) runTemplateUse() (string, error) {
	t, err := r.c.taskByCodeDefault(r.a, "template_5")
	if err != nil {
		return "", err
	}
	n := int64(5)
	if t != nil {
		if need := needFor(*t, 5); need > 0 {
			n = need
		} else {
			return "进度已达标，无需上报", nil
		}
	}
	for i := int64(0); i < n; i++ {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2api-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i)
		tpl := growthTaskTemplates[int(i)%len(growthTaskTemplates)]
		if err := r.c.ReportDesktopEvent(r.a, DesktopTemplateUseSequence(conv, req, tpl[0], tpl[1])...); err != nil {
			return fmt.Sprintf("第 %d/%d 组模板事件上报失败", i+1, n), err
		}
		r.sleep(300 * time.Millisecond)
	}
	return fmt.Sprintf("已上报模板使用事件 %d 组", n), nil
}

// runPlaybookPrompt 灵感案例发送 Prompt 事件组（playbook_prompt）。
func (r *growthTaskRunner) runPlaybookPrompt() (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	events := DesktopPlaybookPromptSequence(conv, req, playbookCaseID, playbookCaseName)
	if err := r.c.ReportDesktopEvent(r.a, events...); err != nil {
		return "", err
	}
	return "已上报灵感案例 Prompt 事件组", nil
}

// runCreateCanvas 设计创意画布事件组（create_canvas）。
func (r *growthTaskRunner) runCreateCanvas() (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-canvas-%d", ms)
	req := fmt.Sprintf("wb2api-canvas-req-%d", ms)
	if err := r.c.ReportDesktopEvent(r.a, DesktopDesignCanvasSequence(conv, req)...); err != nil {
		return "", err
	}
	return "已上报设计创意画布事件组", nil
}

// runAppearance 设置主题 + 上报皮肤生效事件（Hp_Appearance）。
func (r *growthTaskRunner) runAppearance() (string, error) {
	if err := r.c.SetAppearanceTheme(r.a, HpAppearanceThemeKey); err != nil {
		return "", fmt.Errorf("设置主题失败: %w", err)
	}
	r.sleep(2 * time.Second)
	if err := r.c.ReportDesktopEvent(r.a, DesktopSkinApplyEvent(HpAppearanceThemeKey)); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runSkillFresh 真实对话 + skill_info 事件（skill_1）。
func (r *growthTaskRunner) runSkillFresh() (string, error) {
	conv, req, err := r.c.DesktopChatWithExpert(r.a, "")
	if err != nil {
		return "", fmt.Errorf("真实对话失败: %w", err)
	}
	tail := req
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	msgID := "msg-" + tail
	events := DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}
	events = append(events, DesktopSkillInfoEvent(conv, req, msgID, "fast-model", "fast-model"))
	if err := r.c.ReportDesktopEvent(r.a, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件上报失败: %w", err)
	}
	return "已上报真实对话 + skill_info 事件", nil
}

// runExpertBatch5 expert_5：真实召唤并使用 5 个平台专家。
func (r *growthTaskRunner) runExpertBatch5() (string, error) {
	return r.runExpertBatch("expert_5", "agent", 5)
}

// runExpertTeam3 Expert_team_use_3：真实召唤并使用 3 个专家团。
func (r *growthTaskRunner) runExpertTeam3() (string, error) {
	return r.runExpertBatch("Expert_team_use_3", "team", 3)
}

// runExpertBatch 专家召唤 + 使用的公共实现。
//
// expert_id 必须是市场真实条目、requestId 必须是**真实对话的服务端 id**
// —— 自造两者都不入账。故每轮都要：市场列表 → 召唤链 → 真实对话 → actual_use。
func (r *growthTaskRunner) runExpertBatch(code, expertType string, count int) (string, error) {
	t, err := r.c.taskByCodeDefault(r.a, code)
	if err != nil {
		return "", err
	}
	if t != nil {
		if need := needFor(*t, int64(count)); need > 0 {
			count = int(need)
		} else {
			return "进度已达标，无需上报", nil
		}
	}
	experts, err := r.c.MarketExpertList(r.a, expertType)
	if err != nil {
		return "", fmt.Errorf("专家市场不可用: %w", err)
	}
	if len(experts) == 0 {
		return "专家市场返回空列表，跳过", nil
	}
	var done int
	for i := 0; i < count; i++ {
		e := experts[i%len(experts)]
		if err := r.c.ReportDesktopEvent(r.a, DesktopExpertSummonSequence(e)...); err != nil {
			return fmt.Sprintf("召唤链第 %d/%d 个失败", i+1, count), err
		}
		r.sleep(2 * time.Second)
		conv, req, err := r.c.DesktopChatWithExpert(r.a, e.ExpertID)
		if err != nil {
			return fmt.Sprintf("第 %d/%d 个专家真实对话失败", i+1, count), err
		}
		ev := DesktopExpertActualUseEvent(e, conv, req, "craft")
		if err := r.c.ReportDesktopEvent(r.a, ev); err != nil {
			return fmt.Sprintf("第 %d/%d 个 actual_use 上报失败", i+1, count), err
		}
		done++
		r.sleep(3 * time.Second)
	}
	return fmt.Sprintf("已完成 %d 个专家的召唤+使用链", done), nil
}

// runExpertLighthouse Expert_lighthouse：真实召唤轻量云专家并使用（LOCAL 口径）。
func (r *growthTaskRunner) runExpertLighthouse() (string, error) {
	e := MarketExpert{
		ExpertID: expertLighthouseID, ExpertType: "agent",
		DisplayNameZH: expertLighthouseName, ProfessionZH: expertLighthouseName, Version: "1.0.2",
	}
	// 市场列表命中真实条目时以服务端信息为准（version 等）。
	if list, err := r.c.MarketExpertList(r.a, "agent"); err == nil {
		for _, it := range list {
			if it.ExpertID == expertLighthouseID {
				e = it
				break
			}
		}
	}
	if err := r.c.ReportDesktopEvent(r.a, DesktopExpertSummonSequence(e)...); err != nil {
		return "", fmt.Errorf("召唤链上报失败: %w", err)
	}
	conv, req, err := r.c.DesktopChatWithExpert(r.a, e.ExpertID)
	if err != nil {
		return "", fmt.Errorf("真实对话失败: %w", err)
	}
	events := DesktopChatSequence(conv, req, "msg-"+tail8(req), "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = e.ExpertID
			ev["expert_name"] = e.DisplayName()
			ev["expert_industry_id"] = ""
		}
	}
	use := DesktopExpertActualUseEvent(e, conv, req, "LOCAL")
	// 对齐真实样本：轻量云专家 actual_use 的 type 为空、cost=0。
	use["type"] = ""
	use["cost"] = 0
	events = append(events, use)
	if err := r.c.ReportDesktopEvent(r.a, events...); err != nil {
		return "", fmt.Errorf("使用事件上报失败: %w", err)
	}
	return "已上报轻量云专家召唤+使用链", nil
}

// runBlackCat 夜猫子：仅在 23:00–08:00 窗口内补足对话次数。
func (r *growthTaskRunner) runBlackCat() (string, error) {
	if !InNightWindow(time.Now()) {
		return "当前不在 23:00–08:00 计数窗口，本次跳过（区间外上报不计分）", nil
	}
	t, err := r.c.taskByCodeDefault(r.a, "black_cat")
	if err != nil {
		return "", err
	}
	need := int64(1)
	if t != nil {
		need = needFor(*t, 1)
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	for i := int64(0); i < need; i++ {
		if err := r.chatOnce(nightChatModelID); err != nil {
			return fmt.Sprintf("完成 %d/%d 次后中断", i, need), err
		}
		r.sleep(growthTaskGap)
		if err := r.reportChatTimesQuiet("black_cat", nightChatModelID, nightChatModelName); err != nil {
			return fmt.Sprintf("完成 %d/%d 次后上报中断", i+1, need), err
		}
		r.sleep(growthTaskGap)
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", need), nil
}

// reportChatTimesQuiet 上报**一条**带模型字段的对话事件（不做差额计算）。
func (r *growthTaskRunner) reportChatTimesQuiet(code, modelID, modelName string) error {
	cid := fmt.Sprintf("wb2api-%s-%d", strings.ToLower(code), time.Now().UnixMilli())
	return r.c.ReportEvents(r.a, []map[string]any{
		ChatRequestSendEventModel(r.a, cid, "", modelID, modelName),
	})
}

// ---------------------------------------------------------------------------
// 小程序口径动作
// ---------------------------------------------------------------------------

// runMPChatTask mp 对话类任务的通用实现：accept（带回读）→ 按差额补 mp 对话事件。
//
// activityID 非空 = 校园日口径（school_season）；空 = Sequential_Tasks_1/_3。
// 每条事件之间 sleep growthTaskMPChatGap（+0~10s 抖动）—— 连发会被反作弊回滚。
func (r *growthTaskRunner) runMPChatTask(code, activityID string) (string, error) {
	t, err := r.mpTask(code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务（活动可能未开始）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	// 未 accept 时 progress 为 null（target 下发 0）→ 接受后必须回读拿真实 target，
	// 否则会「只补 1 条就误判达标去领奖」，领奖返回 400。
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !r.acceptMPWithVerify(code) {
			return "accept 未登记生效，待下次重试", nil
		}
		if t2, err := r.mpTask(code); err == nil && t2 != nil {
			t = t2
		}
	}
	need := needFor(*t, 1)
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		r.sleep(growthTaskMPChatGap + time.Duration(rand.Int63n(int64(10*time.Second))))
		conv := fmt.Sprintf("wb2api-mp-%d-%d", time.Now().UnixMilli(), i)
		if err := r.c.ReportMPEvent(r.a, MpChatEvent(conv, activityID)); err != nil {
			return fmt.Sprintf("完成 %d/%d 次上报后中断", i, need), err
		}
	}
	return fmt.Sprintf("已上报 %d 条小程序对话事件", need), nil
}

// runSchoolSeason 校园日（带 activityId）。
func (r *growthTaskRunner) runSchoolSeason() (string, error) {
	return r.runMPChatTask("school_season", schoolOpenDayActivityID)
}

// runMPChat1 小程序内完成 1 次对话。
func (r *growthTaskRunner) runMPChat1() (string, error) {
	return r.runMPChatTask("Sequential_Tasks_1", "")
}

// runMPChatN 小程序内完成 N 次对话（target 由任务下发，5 或 10）。
func (r *growthTaskRunner) runMPChatN() (string, error) {
	// 动作表按 code 分派，这里需要知道具体是哪个任务：取两个候选里唯一未完成的。
	for _, code := range []string{"Sequential_Tasks_3", "Sequential_Tasks_6"} {
		t, err := r.mpTask(code)
		if err != nil {
			return "", err
		}
		if t == nil || t.Claimed {
			continue
		}
		if t.Target > 0 && t.Current >= t.Target {
			continue
		}
		return r.runMPChatTask(code, "")
	}
	return "无待完成的小程序多次对话任务", nil
}

// runMPExpert 小程序内使用市场专家（Sequential_Tasks_2）。
func (r *growthTaskRunner) runMPExpert() (string, error) {
	const code = "Sequential_Tasks_2"
	t, err := r.mpTask(code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	// 判据载体前置：**accept 之前**先解析市场真实专家 id，
	// 拉不到就整任务不动作，避免留下「已登记未上报」的半程态。
	experts, merr := r.c.MarketExpertList(r.a, "")
	if merr != nil || len(experts) == 0 {
		return fmt.Sprintf("专家市场不可用（%v），跳过以防半程态", merr), nil
	}
	e := experts[0]
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !r.acceptMPWithVerify(code) {
			return "accept 未登记生效，待下次重试", nil
		}
	}
	if err := r.c.ReportMPEvent(r.a, MpExpertUseEvent(e.ExpertID, e.DisplayName(), e.ExpertType)); err != nil {
		return "", fmt.Errorf("上报 mp expert_actual_use 失败: %w", err)
	}
	return fmt.Sprintf("已上报小程序专家使用事件（%s）", e.DisplayName()), nil
}

// runMPAutomation Sequential_Tasks_4：判据疑为桌面口径，复用 automation_1 同源事件。
func (r *growthTaskRunner) runMPAutomation() (string, error) {
	return r.runMPTaskWithFallback("Sequential_Tasks_4",
		func() error {
			return r.c.ReportDesktopEvent(r.a, DesktopAutomationCreateEvent("omnigate 自动化"))
		}, nil)
}

// runMPModelChat Sequential_Tasks_5：小程序内使用 GLM-5.2。
func (r *growthTaskRunner) runMPModelChat() (string, error) {
	return r.runMPTaskWithFallback("Sequential_Tasks_5",
		func() error {
			conv := fmt.Sprintf("wb2api-mp-glm-%d", time.Now().UnixMilli())
			return r.c.ReportMPEvent(r.a, MpChatEventWithModel(conv, "glm-5.2", "GLM-5.2"))
		},
		func() error {
			return r.reportChatTimesQuiet("mp-glm", "glm-5.2", "GLM-5.2")
		})
}

// runMPPlaybook Sequential_Tasks_7：体验灵感功能。
func (r *growthTaskRunner) runMPPlaybook() (string, error) {
	return r.runMPTaskWithFallback("Sequential_Tasks_7",
		func() error {
			ms := time.Now().UnixMilli()
			conv := fmt.Sprintf("wb2api-pb-%d", ms)
			req := fmt.Sprintf("wb2api-pb-req-%d", ms)
			return r.c.ReportDesktopEvent(r.a,
				DesktopPlaybookPromptSequence(conv, req, playbookCaseID, playbookCaseName)...)
		},
		func() error {
			return r.c.ReportMPEvent(r.a, MpPlaybookEvents(playbookCaseID, playbookCaseName)...)
		})
}

// runMPTaskWithFallback Sequential 链预留任务的通用骨架：
// mp 查询 → accept（带回读验证）→ 主判据上报（未点亮时补一轮备选）→ 回读。
func (r *growthTaskRunner) runMPTaskWithFallback(code string, primary, fallback func() error) (string, error) {
	t, err := r.mpTask(code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务（前置任务未完成或活动未开始）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !r.acceptMPWithVerify(code) {
			return "accept 未登记生效（可能处于每日锁定窗口，等解锁后自动重试）", nil
		}
	}
	if err := primary(); err != nil {
		return "", fmt.Errorf("判据上报失败: %w", err)
	}
	return "已上报判据事件（判据形态待实测校正）", nil
}

// mpTask 按小程序口径读取单个任务。
func (r *growthTaskRunner) mpTask(code string) (*Task, error) {
	tasks, err := r.c.ListTasksMP(r.a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// acceptMPWithVerify 接受小程序任务并回读验证（上游存在「200 但未落账」形态）。
func (r *growthTaskRunner) acceptMPWithVerify(code string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		if err := r.c.AcceptTasksMP(r.a, []string{code}); err == nil {
			r.sleep(growthTaskMPActionGap)
			if t, err := r.mpTask(code); err == nil && t != nil &&
				t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 调度器接入（可选接口探测，见 internal/scheduler/growth.go）
// ---------------------------------------------------------------------------

// GrowthTasksSupported 报告本渠道是否提供成长中心。
//
// 现状：仅 WorkBuddy 国内版提供。调度器据此在其它渠道上**静默跳过**，
// 避免把「渠道不支持」误记为账号故障。
func (c *Client) GrowthTasksSupported() bool { return true }

// GrowthRunTasksOnce 调度器入口：对单个账号执行任务动作表并返回摘要。
//
// 只返回**基本类型**，不暴露 GrowthTaskReport —— 调度器不 import 本包的结构体。
// 返回的 err 仅在「整体性失败」时非 nil（如任务列表都拉不到）；
// 单项失败已汇总进 msg，不会让调度器把它当成账号故障。
func (c *Client) GrowthRunTasksOnce(a *auth.Auth, includeMP bool) (string, int64, int64, error) {
	rep := c.GrowthRunTasks(a, GrowthTaskOptions{IncludeMP: includeMP})
	if rep.Err != "" {
		return "", 0, 0, fmt.Errorf("%s", rep.Err)
	}
	msg := rep.Msg
	if n := len(rep.Tasks); n > 0 {
		msg = fmt.Sprintf("%s（%d 项）", msg, n)
	}
	return msg, rep.Credit, rep.Energy, nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// InNightWindow 报告是否处于夜猫子计数窗口（23:00–08:00）。
func InNightWindow(t time.Time) bool {
	h := t.Hour()
	return h >= 23 || h < 8
}

// tail8 取字符串末 8 位（用于构造 messageId）。
func tail8(s string) string {
	if len(s) > 8 {
		return s[len(s)-8:]
	}
	return s
}

// progressText 进度文本，如 "3/5"；target 未知时只显示当前值。
func progressText(t Task) string {
	if t.Target <= 0 {
		return fmt.Sprintf("%d/?", t.Current)
	}
	return fmt.Sprintf("%d/%d", t.Current, t.Target)
}

// taskTitle 任务的展示名（标题缺失时退回描述或任务码）。
func taskTitle(t Task) string {
	if t.Title != "" {
		return t.Title
	}
	if t.TaskDesc != "" {
		return t.TaskDesc
	}
	return t.TaskCode
}

// containsCode 判断 code 是否在列表中（大小写敏感，与上游一致）。
func containsCode(list []string, code string) bool {
	for _, s := range list {
		if strings.TrimSpace(s) == code {
			return true
		}
	}
	return false
}

// summarize 汇总报告为一句人话。
func summarize(rep GrowthTaskReport) string {
	var claimed, done, failed int
	for _, t := range rep.Tasks {
		switch t.Status {
		case "claimed":
			claimed++
		case "done":
			done++
		case "error":
			failed++
		}
	}
	var parts []string
	if rep.Credit > 0 || rep.Energy > 0 {
		parts = append(parts, fmt.Sprintf("到账 +%d 分 +%d 能", rep.Credit, rep.Energy))
	}
	parts = append(parts, fmt.Sprintf("领奖 %d 项、完成 %d 项", claimed, done))
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("失败 %d 项", failed))
	}
	if n := len(rep.Manual); n > 0 {
		parts = append(parts, fmt.Sprintf("另有 %d 项需在客户端手动完成", n))
	}
	return strings.Join(parts, "；")
}
