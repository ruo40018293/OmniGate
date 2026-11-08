# OmniGate 开发约定（AGENTS.md）

> 本文件面向 AI Agent 与开发者，记录**工具设计、架构选型、决议项**与开发约定。
> 决议项（第 1 节）记录的是**为什么这么做**，改动前请先读对应条目 ——
> 多数看起来「可以简化」的写法都有明确的踩坑记录。

---

## 0. 项目一句话

多平台（Windows / macOS / Linux）多渠道账号聚合工具：把各家平台的多个账号聚合成
一个 OpenAI 兼容 API，管理界面走**系统托盘 + 系统浏览器 Web UI**。

形态上刻意不用 Wails / WebView2：省内存与运行时依赖，托盘常驻、面板用浏览器打开，
因此面板可以在任意设备上访问。平台能力封装在 `internal/platform`（build tag 拆分）。

## 0.1 新增渠道的完整清单（最易漏，漏了不报错）

接入一个新渠道需要改**五处**，其中第 4、5 处漏了**不会报错**，只是该渠道的
自动签到/保活从不运行或界面无入口 —— 属于最难自查的一类缺陷：

1. `internal/<channel>/` 协议封装，实现 `provider.Upstream`
2. `internal/login_<channel>/` 凭据加载（面板登录 / 导入）
3. `cmd/omnigate/main.go` 装配 `Runtime`、`client`、代理同步项
4. `cmd/omnigate/main.go` 调度器启动处补 `go <sch>.Run(sctx)`
   —— 文件里有醒目注释标注此位置
5. 前端 `cmd/omnigate/web/` 加渠道展示与操作入口

另有渠道前缀白名单（`internal/stats/stats.go`）需同步新增，否则统计漏该渠道。

## 1. 已定决议项（不要推翻，除非有强理由并更新本节）

> 本节**每条只留一句话判据**；完整背景、踩坑记录与边界条件在 `docs/DECISIONS.md`（同名编号逐一对应）。
> 改某条之前先读它的原文——多数「看起来可以简化」的写法都有明确的翻车记录。
> 新增决议：本节加一行 + `docs/DECISIONS.md` 加原文，**编号必须一致**。

| # | 一句话判据 |
|---|-----------|
| R1 | 托盘菜单固定，不做动态内容、不做定时/事件刷新 |
| R2 | 托盘不再提供「刷新积分」菜单，刷新积分一律走 Web 面板 |
| R3 | 托盘固定三项：打开主界面 / 查看日志 / 退出；双击托盘 = 打开主界面 |
| R4 | 管理 API 采用 cookie 会话鉴权 |
| R5 | Web UI 用纯静态 HTML/CSS/JS |
| R6 | 托盘库保留 energye/systray（已跨平台），菜单项图标用纯 Go 生成、不引外部图标文件 |
| R7 | 移除 wails / WebView2 全部依赖 |
| R8 | daemon 单进程：一个 http.Server 同时服务 OpenAI 端点 + 管理 API + 静态 UI |
| R9 | 核心业务整体复用、格式零迁移：config.json / auths/ / data/state.json 兼容旧版 |
| R10 | 新增渠道 = 实现 provider.Upstream + auth 加载器 + 注册 Runtime（前缀路由 channel/model） |
| R11 | Windows 产物 WSL 交叉编译（GOOS=windows CGO_ENABLED=0）；macOS 走 GitHub Actions（cgo 必需） |
| R12 | 无桌面 Linux 使用 `--no-tray` 参数 |
| R13 | 三接口兼容采用两层结构：内层 handler 不动，新增 `internal/gateway` 边缘层 |
| R14 | `Stream`/`Aggregate` 的 model 由调用方显式传入 |
| R15 | Responses 的 `function_call` 必须是独立 output item |
| R16 | 无账号渠道不建 auth 文件、不进 `reloadAccounts`、不参与禁用/冷却惩罚 |
| R17 | 用量/积分双流水分口径统计，不强关联、不折算 |
| R18 | 临期阈值可配（默认 24h，下限 24h） |
| R19 | TraeWork 专用池判据仅 `available_endpoint==1` |
| R20 | 千问办公推理 body 必须携带 `business` 段 |
| R21 | 千问办公 `expires_in` 单位是秒 |
| R22 | 智谱清言（`glm/*`）走网页版私有接口；登录以 CDP 自动捕获为主、手工粘贴为兜底 |
| R23 | 清言「签到」的实质是保活对话；新账号额度需 App 侧登录才发放 |
| R23b | 分析纪律：观察性数据只能提假设，定因果必须做对照实验 |
| R24 | 清言渠道为正常多账号渠道（`SingleAccount` 不设） |
| R25 | 清言 SSE 是「增量 delta」，但 `part.status=="finish"` 时给的是全文 |
| R26 | 清言 `accessToken` 允许为空（凭据本体是 refresh_token） |
| R27 | 清言登录走 CDP 自动捕获 Cookie，独立 profile，手工粘贴仅作兜底 |
| R28 | 自动登录的完成判据是「凭据验证通过」，不是「Cookie 出现」 |
| R29 | 多账号轮换的真实语义：粘性路由 + 错误惩罚 + 同请求换号 |
| R30 | 清言「伙伴/群聊」任务不自动化 |
| R31 | 每日登录积分走 `member-api/member/daily_login_score`，已接入保活流程 |
| R32 | 不模拟 App 登录来触发额度发放 |
| R33 | 三个 chatglm 变体的模型名带上游代号后缀 `:moe_53f`；旧名保留为别名 |
| R34 | 新增渠道必须补 go xxxSch.Run(sctx)，否则该渠道的自动签到/保活从不运行 |
| R35 | 流式请求必须用独立的 StreamHTTP（不设 Client.Timeout） |
| R36 | 流被截断不得靠补 `data: [DONE]` 伪装成正常收尾 |
| R37 | TraeWork AuthCode 交换：4xx 属终态，不得换 origin 重试掩盖首因；设备号作用域 = 账号 |
| R38 | 上游按模型限流时不得软化整个账号 |
| R39 | `scheduler.Config` 的时间字段：`nil` = 未配置（落默认），`[]int{}` = 本渠道没有这类定时任务 |
| R40 | 流内业务错误不得伪装成正常收尾；兼容层不得吞 error 帧 |
| R41 | 小浣熊协议回调必须由 `main.go` 在初始化前拦截（`--raccoon-callback`） |
| R42 | 渠道本地模型校验必须取「静态表 ∪ 动态目录」的并集 |
| R43 | 运行统计（`internal/stats`）与用量流水（`internal/ledger`）是两套并列口径，不合并、不互校 |
| R44 | WorkBuddyAI 对话必须带 `X-Conversation-ID`（三件套缺一不可） |
| R45 | 「模型繁忙」（429 + `code:14003`）只重试、不冷却 |
| R46 | QoderCN「模型排队」（`10605`/`isQueued`）必须识别为错误，不得透传或补 `[DONE]`；并以首帧看门狗兜住上游挂起 |
| R47 | 主 HTTP 服务必须显式设置 `IdleTimeout`（60s） |
| R48 | 平台表 grid 轨道数必须与 JS 每行单元格数同步 |
| R49 | CN workbuddy 对话头补齐 X-Conversation-ID，并留 OMNIGATE_CAPTURE_CHAT 出站捕获钩子 |
| R50 | CN workbuddy 渠道对 DSH 运行时上下文两句连排字面回 11128（"You are interacting with the user through the DeepSeek Harness Web GUI…"+"…… |
| R51 | 渠道身份链必须与官方桌面客户端逐字一致 |
| R52 | 两份脱敏表必须同步，且必须用真实原文断言 |
| R53 | DSH 身份句整段删除（不再插冠词），并修掉预检门的假阴性 |
| R54 | 出站取证从「全量 env 门控」改为「异常自动触发」，11 渠道全覆盖 |
| R55 | qodercn 深排队：熔断读写两侧必须共用同一取键函数；重投节奏按上游 waitTime（不许固定间隔） |
| R56 | 等待期必须可感知：受理帧 + 实时面板 + 失败原因入统计行 |
| R57 | qodercn 排队重投节奏按上游 waitTime 而非固定 30s + GC 关停同步 |
| R58 | qodercn 排队保活上限压回 5 分钟，熔断窗必须严格小于它 |
| R59 | CN workbuddy 渠道 11128 复发：DSH 运行时上下文的 @path 说明句被上游当渠道校验特征 |
| R60 | 平台表每轨必须有 min 地板（裸 1fr/auto 必错）、末列 ≥84px；宁可横滚，不裁字 |
| R61 | ~~raccoon 无领取端点：每日 300 分靠「当天首次触达」懒发放，签到 = 摸一次余额 + 核对当日流水~~ **已被 R98 推翻**：balance/bills 只读、根本不触发发放，真正触发点是 `setting_info`，见 R98 |
| R62 | 启动时绑端口失败必须直接退出，不得留空壳进程 |
| R63 | MiniMax/Cline 两渠道移植自 wild-work（MIT），七处接线缺一即「面板正常但某处整组缺失」 |
| R64 | usage 数值类型必须走 asInt64（覆盖 float/整型/json.Number），只认 float64 会把 int64 静默记成 0 |
| R65 | 接新渠道是六个登记点 |
| R66 | 面板「运行统计」必须能区分输入与输出 Token；「今日情况」也要给输出维度 |
| R67 | 面板 Token 要把输入维度也显式给出；平台表单元格的「入/出」必须真的分行 |
| R68 | 签到遇 401 刷新后重试必须退避，不能立刻重打 |
| R69 | 7 日/30 日「输出」窗口必须是真实历史，不能恒等于今日 |
| R70 | 运行日志要轮转，但「今天写过的日志」绝不能搬走 |
| R71 | 模型表积分列折算不出来时显示「—」，不得显示 0（0 会被读成免费） |
| R72 | 面板 Token 一律「输入/输出/合计」三档给全，只有合计就是在骗人（占比 99:1 时看不出谁在吃额度） |
| R73 | 面板轮询要按「tab + 可见性」门控，轮询专用信息不拉全量状态（/api/state 一项曾占全部访问 34.7%） |
| R74 | 批量作业必须「立刻受理 + 进度可查」，同一作业槽拒绝并发（同步等 122.99s = 点了没反应） |
| R75 | 取证目录按「文件数 + 总字节」双上限修剪，从最旧删起（只封文件数挡不住 400MB） |
| R76 | 成长中心的「手动执行」必须与「定时开关」解耦：能自动跑的动作要有单账号按钮，点了就执行（开关只管定时） |
| R77 | 发版链路只认 vX.Y.Z 且必须**幂等**：`v*` 通配会把 `version` 这类标签带进来，`gh release create` 撞同名 release 必挂 |
| R78 | 改名要带兼容层与迁移：新名优先、旧名兜底、数据目录搬过去挪（硬切换 = 存量部署升级即丢配置） |
| R79 | 图标标志必须是小尺寸可辨的单一形状（当前=门：拱+门槛，门槛比门框宽），改图标必跑 `build/check_icon.py` 的断言，且 ico→托盘/logo64/appicon/syso 四处同源刷新 |
| R80 | 发布说明里写的文件名必须等于真实产物名（`OmniGate-<ver>-{amd64,arm64}.dmg`），不写 `<arch>` 占位符（GitHub 当 HTML 标签吃掉）；由 `internal/app/release_notes_test.go` 守 |
| R81 | README 里的资产名同样是契约：必须指向真实文件、不写 `<arch>` 这类尖括号占位符（用 `{版本}`）、环境变量前缀写 `OMNIGATE_*`（旧 `WILDWORK_*` 兜底）；由 `internal/app/release_notes_test.go` 的 `TestReadmeArtifactNamesMatchReality` 守 |
| R82 | 被内容策略拦截（`11-128`）时把发往上游的原始请求体整包落盘到 `data/blocked/blocked-<unixnano>.json`（滚动保留 3 份，最多 2 MB），供 `scripts/bisect-blocked.py` 二分重放定位指纹；由 `internal/upstream/diag_test.go` 守（元凶在首条 system 的句子前缀里，面板/日志不必再猜） |
| R83 | 指纹检测必须容忍大小写与分隔符变体：写死 ASCII 的预检和写死的 billing header 键名正则会漏掉全大写、U+2011 连字符、空格、零宽空格等变体，且 messages 树遍历不到 tools/metadata，需要 BodyFallback 整包兜底；漏检会让请求裸奔出网被网关判 code=11-128。由 internal/sanitize/{fold,bodyfallback,loosekey}_test.go 与 internal/upstream/{fold,bodyfallback}_test.go 锁住，输入一律用字节或 hex 构造 |
| R84 | 脱敏规则的匹配键必须是**线上原文形态**，不能是「改写后形态」：生产表 rewrites[7] 的 old 曾写成 this console（已改写形态），而真实请求里是 this GUI（抓包 45 处/18 个文件），该规则从不命中、尾句裸奔出网；现有跨包 parity 测试只断言 221 字符连排整串被打断，首句改写即可让它通过，故洞被全绿地藏着。由 internal/sanitize/r84_tail_test.go 与 internal/upstream/r84_tail_test.go 以 base64 内嵌原文、单独放尾句的方式锁住（判据已被 R86 修正：45 处命中是对话正文，不是运行时上下文） |
| R85 | 逐规则 parity 守卫：跨包 parity 测试只断言「221 字符连排整串被打断」，首句规则一生效整串就断，尾句规则即使完全失效也照样全绿（R84 的洞就是这么藏住的）。`internal/upstream/r84_guard_test.go` 把两份表的 9 条非空 old 键逐条单独喂入两侧，断言每条键在 upstream 与 sanitize 都必须被改写；输入一律 base64 内嵌、JSON 由 `%c` 字节级拼装（文件内零反斜杠）。变异：撤销 R84 → key7 红；upstream idx6 置为空转 → key6 红 |
| R86 | 脱敏链只覆盖 `internal/upstream`（CN 两段式）与 `internal/workbuddyai`（单段式），其余渠道原本全不在链上；CN 的 stage2 因 stage1 输出已无 feature 命中而**恒为 no-op**（改 `internal/sanitize` 的表对 CN 出站无影响）；R84 的「尾句裸奔」判据按 role 重算后证伪（workbuddy 14/15 带运行时上下文、system 漏 0；raccoon 8/8、traework 2/2 漏但零拒信）。raccoon/traework 已接入，traework 的脱敏必须早于协议转换；由 `internal/{raccoon,traework}/r86_sanitize_test.go` 守 |
| R87 | CN 脱敏是**两段式且 stage1 是承重墙**：`PrepareBody = BodyFallback(Messages(prepareBodyInner(src)))`，而 `prepareBodyInner`（`internal/upstream/payload.go:65`）已用 upstream 表做过一遍 `sanitizeMessages`；stage1 生效后 stage2（`internal/sanitize` 表）的 `hasFingerprint` 预检不再命中 ⇒ **恒为 no-op**，CN 出站的实际改写形态由 upstream 表决定。两份表的**匹配键**已由 R84 对齐，**替换目标形态仍不同**（upstream 等价词 vs sanitize leetspeak）；删 stage1 会把 CN 出站字节改成从未在生产验证过的 leetspeak 形态，属未声明的行为变更。`internal/workbuddyai` 的 `prepareBodyInner` 没有 stage1，故国际版由 sanitize 表单独生效。由 `internal/upstream/r87_two_stage_test.go` 守（变异：`sanitizeMessages(msgs)` → `_ = msgs` 立刻红） |
| R88 | 本地判定的请求错误不得用 404：`internal/cline` 的 `knownModel` 拒绝原先返回 404，被 `Classify` 归成 `ErrNotFound`（`Rotatable()`）⇒ handler 换号 + `NoteError`，客户端打错一个模型名就能把整条 cline 渠道拖进冷却（上级项目 2026-10-28 同类事故实记：两个号各记两笔后整渠道冷却）。改法两件必须同时做：状态码 400 + 响应体带专属文案命中 `badParamMarkers` ⇒ `ErrBadParams`（不轮换、不罚号、原文透传）；只改状态码不够——`ErrClient` 也在轮转白名单里。识图能力同时按实测表纠正：`sn-glm-5-3` 实测不支持 `image_url` 却带 vision 标签（误标），`raccoon-19b265`/`raccoon-405a1c` 漏标但能读图；静态表旧 ID `sn-deepseek-v4-1-flash` 按生产日志（1372 次全是 `-1017`）更正。闸门：`internal/cline/r88_local_model_test.go`、`internal/raccoon/r88_vision_test.go`（四处变异全红） |
| R89 | `/v1/models` 必须把上游声明过的思考档位 / 上下文档位声明出去：审计发现渠道侧其实**能收** `reasoning_effort`（qoder 系、minimax、loomy、monkeycode），但声明侧一个字段都没有（实测 212 个模型：`reasoning_efforts`/`default_reasoning_effort`/`can_disable_thinking` 各 0 条）⇒ 客户端无法渲染下拉框，用户「没得选」。改法：`provider.ModelInfo` 新增 `ContextWindows`；`buildModelEntry` 输出 `reasoning_efforts`（len>0）、`default_reasoning_effort`（非空）、`can_disable_thinking`（指针非 nil，false 与 nil 不同义）、`context_windows`（**>1 才输出**，单档不算选择）；qoder/qodercn/qodercom 把 `context_config` 档位表拷进 `ContextWindows`，loomy 把 `/models` 的 `reasoning_efforts`/`default_reasoning_effort` 带进 `ModelInfo`。**不编造是硬约束**：给 qoder 补 low/medium/high 就是替上游做主。闸门：`internal/server/r89_capability_declare_test.go`、`internal/{qoder,qodercn,qodercom}/r89_context_windows_test.go`、`internal/loomy/r89_reasoning_efforts_test.go`（五处变异全红） |
| R90 | 流式错误必须带着原因出去、必须能被惩罚：实测生产日志 `qodercn err=upstream stream truncated` 47 条 + `qodercom` 30 条，出口一律是固定串 「upstream stream truncated」（底层 err 被丢弃），客户端只能看到「断了」，180 秒后才收场；另 `qwenwork err=upstream 402` **118 条**额度耗尽却不冷却——因为流内错误帧走的是裸 `fmt.Errorf`，实现不了 `provider.StreamErrorClassifier`，handler 的 `errors.As` 取不到 kind、落 default「不罚账号」，粘性路由把下一次请求送回同一个刚撞完额度的号。改法三条：① `internal/provider/stream.go` 补齐 `TruncationCause`/`TruncationError`/`TruncationCode`/`TruncationFrame`/`WriteTruncationFrame`，`IsIdleTimeout` 还要认 `context.Canceled`/`DeadlineExceeded`/`net.ErrClosed`（否则「我们主动掐的」与「上游真断的」同形，R58 就卡在这）；② 四渠道流内错误帧统一成 `sseError{status,msg,retryAfter}` 实现 `Kind()`，出口 code `upstream_quota_exhausted`/`_rate_limited`/`_session_dead`/`_model_blocked`/`_business_error`/`_server_error`，信封 `statusCodeValue>=400` 在 onChunk **之前**早返回；③ 四渠道 `parseNestedSSE` 的 EOF 判定：`!outerOK` 一律判截断（外层 JSON 非法 = 帧断在半路），`err==io.EOF && !innerOK` 判截断，末行空判定改用 `lastNonBlankLine`（`ReadString('\n')` 会把「最后完整帧自带空行」一并交回，`TrimSpace` 判不出来，会把干净收尾误判成截断）。闸门：`internal/qodercn/r90_stream_error_test.go`（含 `TestTruncationErrorKeepsCause` 直锁原因保留）、四渠道 `TestStreamTruncationNotDisguisedAsDone` 各补一条「干净 EOF 但末帧不完整」用例；八处变异全红 |
| R91 | 渠道指纹与会话头必须按上游**真实语义**生成，不按「能过校验」生成：① [raccoon] chat 请求此前**完全不发 `User-Agent`**（Go 默认 `Go-http-client/1.1`），也没有 `x-client-*` 家族；上游 2026-10-31 反编译「小浣熊 1.0.38 → box-agent-acp」证实**对话请求走 Python 子进程**（不是 Electron 渲染层），真实头集 = `AsyncOpenAI/Python 2.8.0` + `X-Stainless-{Lang,Package-Version,OS,Arch,Runtime,Runtime-Version}` 六件 + `x-client-{name,platform,version,os-version,channel}`（`device_id` 官方从不赋值 ⇒ 不发）；并且「每请求新建 `sess-<hex>`」会让上游看到同一账号几分钟内冒出上百个新会话 —— **这是 429 的真正触发条件**（官方一个 ACP 会话跨 37 分钟、150+ 次调用都不换）⇒ `internal/raccoon/{headers,requestid,osversion_windows,osversion_linux,osversion_other}.go` 落地 `ApplyHeaders` + 24h 会话池（key = `账号|模型`，必须带账号否则会话跨号漂移）。② [workbuddy CN / WBAI] 两区的 `X-Conversation-ID` 此前也是每请求现生成，官方语义是三层：**会话级恒定 / 轮级（一次 user send）/ 调用级（每模型请求）** ⇒ `internal/upstream/convheaders.go` 与 `internal/workbuddyai/convheaders.go` **两份平行实现**（锚点 = 首条 user 文本 sha256 前 8 字节派生 conv、末条 user 指纹派生 turn，`maxAnchorBody = 8<<20` 超限退回随机），`newMessageID` 由时间戳派生升级为 crypto/rand。闸门：raccoon 10 个 + upstream 12 个 + WBAI 4 个测试；七处变异全红；顺带修掉上游 `randomUUIDv4` 的切片 bug（产出 4-4-4-4-16 畸形串，不是任何 UUID 形态） |
| R92 | 传输层瞬态错误必须**同号原地重试**，且只在最终失败时留证：2026-11-03 23:21:37 与 23:23:43 两次「任务停下来」实测都落在 traework 的 `dial tcp: lookup trae-api-cn.mchost.guru: no such host`（`data/chat-capture/err-20261103-232343.287-traework-0-527031.json` 里 `status=0`/`response=""` ⇒ 请求根本没发出去）。全仓 12 个渠道的 chat `Do` 一次失败就放弃（唯一例外是 workbuddyai 的 `retry.go`）；而 `auths/` 实测**每渠道只有 1 个账号** ⇒ 传输失败时无号可换，只能同号重试。日志统计 chat 路径 transport error 共 21 条（`no such host` 8 / `timeout awaiting response headers` 9 / `dial tcp i/o timeout` 3 / `wsarecv` 1），逐条比对「失败后下一次成功的时间」除一次当天渠道不可用外**全部 1–16 秒内恢复** ⇒ 短退避重试即可吃掉这类抖动。新增 `internal/httpretry`（`Do(req, do, wrap, onRetry)` = 3 次尝试、400ms 起指数退避 ±40% 抖动、`Retryable` 判据先拆 `*url.Error` 再判，否则本地 `invalid header field value` 会被当成瞬态；用 `GetBody`+`Clone` 重放且**不可重放就只发一次**，绝不发空 body；失败返回 `noopCancel` 而非 nil，否则渠道侧错误分支的 `cancel()` 直接 panic），接进 traework / workbuddy(upstream) / qoder / qodercom / qwenwork / raccoon / minimax / loomy / monkeycode / glm 十处；顺带修掉一个**既有时间炸弹**：`internal/raccoon/checkin_test.go` 用 `now.Truncate(24*time.Hour)` 取「当天零点」，Truncate 按纪元纳秒对齐⇒ CST 00:00–08:00 之间它给的是**昨天** 08:00，三条签到用例在凌晨必然集体变红（2026-11-04 00:22 实测复现），改 `cstMidnight`。闸门：`internal/httpretry/httpretry_test.go`（9 例）+ `internal/traework/r92_retry_test.go`（4 例），五处变异全红 |
| R93 | 流式请求必须给**首内容**设截止；日额度耗尽不是「请求错误」：2026-11-04 生产日志实测 qodercn `403 Billing daily count exceeded` 一天 1 次，而 12 次 `upstream stream truncated`（qodercom 9 / qodercn 3）全部是「上游只发一帧 role 就沉默、约 180 秒后自己断开」（R90 实测 p50=180.1s，写给客户端的字节只有 81~513）⇒ 用户在界面上干等三分钟，体感就是「卡住了」。两处根因：①没有首内容截止，`newAcceptWriter` 的受理帧让人以为在跑；②`403` + 非空 body 落到 `ErrClient`（在 `Rotatable()` 白名单里）⇒ 单号渠道换号必败 ⇒ 最后只剩一个无信息量的 503。改法：①`internal/server/contentwatch.go` 新增 `contentWatcher`（按**出站 SSE 帧**识别首帧非空内容，`content:""` 的 role 首帧不算内容；`content` / `reasoning_content` / `tool_calls` 三种形态都算，`[]`/`null` 不算；`Flush` 必须转发否则 SSE 退化）+ `firstContentReader`（到点先**毒化**再关底流，否则读取方拿到 `io.ErrClosedPipe`，首内容超时又和上游真断同形）；handler 在 `FirstContentTimeout`（`config.stream_first_content_seconds`，缺省 90s、负数关闭、上限 600s）到点断流，渠道按 R90 路径写 `upstream_timeout` 帧；新增 `provider.ErrFirstContentTimeout` 并让 `IsIdleTimeout` 认得它。②8 份平行 `hardMarkers`（qodercn/qoder/qodercom/qwenwork/upstream/workbuddyai/raccoon/loomy）统一补 `billing daily count exceeded` / `daily count exceeded` / `次数已达上限` ⇒ 日额度耗尽归 `ErrHardCredit` 冷却到日重置。闸门：`internal/server/r93_contentwatch_test.go`（4 例含沉默上游端到端）+ `internal/qodercn/r93_daily_quota_test.go`（8 例）+ `internal/provider/r93_quota_markers_test.go`（8 份表一致性，读源文件——包内私有变量无公共接口可断言），七处变异全红
| R94 | 渠道语义不能被全局设置覆盖：上游 v2.11.1→v2.11.5 盘点后逐条对着我们的代码核，三条是**逐字同型**的真 bug。① [Anthropic 入站工具有序性] `internal/gateway/anthropic.go` 把文本/图片 **prepend** 到 tool 结果之前，出站顺序变成 `user(文本) → tool → tool`，违反「`assistant.tool_calls` 之后必须紧跟同批 `role:tool`」；上游 workbuddy 回 `11148 tool calls and tool results do not match`，qoder 系只回信封 `Error in upstream response`（无信息量）。触发是常态：Claude Code 在工具执行完、用户又追一句时，把 `tool_result` 与新文本放在同一条 user 消息里。改法：文本/图片 `append` 到结果**之后**，纯工具结果不再凭空补一条空 user。② [签到三根因] `cmd/omnigate/web/app.js` 先判 `last_checkin_at` 再看 `noCheckin` ⇒ 无签到渠道的灰色分支**永远不可达**（巡检每轮都写时间戳）；`internal/app/app.go` 的 `SetCheckinTimes` 无条件给**所有**渠道 `SetCheckinMinutes` ⇒ Qoder 双区（固定 10:00 开放）与 MiniMax（00:00 重置积分）的固定窗口被面板时刻覆盖，**当天直接漏领**；`internal/scheduler/scheduler.go` 的 `finishCheckin` 没有「本渠道没有签到任务」闸门 ⇒ 无签到渠道每天落一条失败记录。改法：`scheduler.Config.FollowGlobalCheckin` + `FollowsGlobalCheckin()`，面板时刻只推白名单（workbuddy/traework/workbuddyai/raccoon/glm）；`finishCheckin` 在 `len(CheckinMinutes())==0` 时既不落盘也不上报；前端先判 `noCheckin`。③ [Loomy 到期] `importLoomy` 写 `updatedAt + 14d`，而 `auth.NeedsRefreshLocked` **只看 ExpiresAt、不看 refreshToken** ⇒ 第 15 天起恒真 ⇒ `loomy.RefreshToken` 恒 `ErrSessionDead` ⇒ 面板红字 + 该号被跳过 + 每 10 分钟一次必然失败的刷新。上游实测反证：某号 `expiresAt` 已 2026-10-27 过期，10-30 仍 `POST /api/account/resource_detail` 200（remain=17518）⇒ 14 天估算模型不成立，真实失效只能靠 401 兜底。改法：写 50 年哨兵（与同文件 `importMonkeyCode` 同一约定）+ `healLoomyExpiry()` 启动自愈（幂等、只改 `auth.expiresAt`、坏文件跳过不阻断启动）。闸门：`internal/gateway/r94_anthropic_tool_order_test.go`(4) + `internal/scheduler/r94_checkin_test.go`(4) + `internal/app/r94_checkin_push_test.go`(2) + `internal/app/r94_loomy_heal_test.go`(4) + `internal/app/r94_main_wiring_test.go`(1，源码级白名单闸门：谁被加上 `FollowGlobalCheckin` 就转红)；六处变异全红。**未做**：R151 sanitize 收敛（上游停用 3 条我们仍靠着的规则，必须自建探针才能判）、R147 吞吐 60s 滑窗、R112b 临期展示 1/2/7 可配 |
| R95 | 脱敏按角色收敛、整包兜底删除：照上游 v2.11.5 的 R151（2026-11-02→03，真机探针 33+4+26 次）重验我们的判据，我们三条都做错了。① [篡改用户数据] `cleanMessages`/`sanitizeMessages` **不分角色**，`role:user` 的内容照样被改写（实测 `You are Claude Code, Anthropic's official CLI for Claude` → `...official CLI tool for Claude`）；上游结论是 **user/tool/function 不扫，system/developer/assistant 仍拦**——用户完全可能正常提到这些词，改写等于篡改调用方数据。② [整包兜底不可逆] R82 照抄的 `BodyFallback` 用整包正则替换，实测把 `tools[].description` 里的品牌串改成**另一家厂商的名字**（`search the deepseek harness docs` → `search the Claude Code docs`），而工具定义是调用方的长期资产，这种改写不可逆；上游 R151 用探针确认 tools 字段已不再触发拦截，**整包兜底直接删除**。③ [预检不敏感但替换敏感] 特征表是小写全串、`hasFingerprint` 用 `asciiContainsFold` 预检，但替换走 `strings.ReplaceAll`（大小写敏感）⇒ 分隔符变体（`deepseek-harness`/`deepseek_harness`）连净化都进不去。改法：两个包各自新增 `sanitizeRole` 角色闸门 + `identityRe`（`(?i)you\s+are\s+an\s+ai\s+agent\s+powered\s+by\s+(?:the\s+)?deepseek[\s\-_\x{2010}-\x{2015}\x{00a0}]*harness\.?`）并让 `hasFingerprint` 认它；`cleanText`/`sanitizeText` 改成**命中才 TrimSpace**（无改写不吃首尾空白）；删 `BodyFallback`+`dshReplaceTarget`，两处接线（`internal/upstream/payload.go`、`internal/workbuddyai/wb_utils.go`）回到纯 `sanitize.Messages`。闸门：两个包各 `r95_role_test.go`（4 例：调用方角色原样保留 / 提示词角色全变体中化且不伤前后文 / 整包改写不得复活 / 命中未改不吃空白），五处变异全红（含 upstream 平行实现那一处，防两表漂移）。**未做**：R151 把身份句换成 `powered by DSH`，我们沿用 R53 的**整句删除**（不留替代断言，更干净）；`tool_calls.arguments` 内的品牌串上游也未重验 |
| R96 | 「倍率 0」有两种，显式 `x0.00` 是**定价信息**，不许丢：`internal/upstream/client.go` 的 `FetchModelPricing` 原先按 `rate<=0` 一刀切跳过，把上游显式 `"x0.00 credits"`（`hy4free` / `hy3` 的「限时免费」）与「压根没有 credits 字段的不计费内部模型」（`hunyuan-chat` 等）混成一件事 ⇒ 免费模型从费率表消失、`/v1/models` 少了 `(0.00x)` 后缀、面板按「上游未返回倍率」默认隐藏，用户在网关侧根本看不出哪个模型免费（只能在官网看到「限时免费」）。改法：`explicit := strings.TrimSpace(m.Credits) != ""`，只跳过 `rate<=0 && !explicit && id != "auto"`；下游 `FeesInfo` 的 `Free = IsExplicit() && Rate == 0`、前端 `rate-free → ✦ Free`、`RateSuffixFormat(0,true) = " (0.00x)"` 都已就位，缺的只是入口这一层。闸门：`internal/upstream/r96_free_pricing_test.go`（显式 0 保留 / 缺字段跳过 / auto 非显式 / 后缀形态），变异回 `rate<=0` 立刻红 |
| R97 | 临期「花费」必须是**净花费**，且「到期作废」与「消耗速度」必须分开：上游 PR #217 修复的逐字同型缺陷——`creditOut` 是余额下降差值，不区分花掉与到期作废，昨领 300 今晨整包作废、白天又领 300 时 `creditOut=300` 全是作废、真实花费 0，旧口径外推出「已耗 300 · 预计作废 264」自相矛盾。改法：`internal/stats/stats.go` Version→2，`ExpiryRow` 加 `today_spent`/`today_expired`，新增 `expiredByUID` 与 `netSpentLocked`（`max(0, creditOut − 今日该 uid 作废)`），`burnForecastLocked` 改用净花费；`ApplyLedgerExpire` 签名由 `map[string]int64` 改 `[]LedgerExpire`（逐条带 Channel/UID，空 UID 只进渠道维度），`internal/app/stats_feed.go` 逐条喂入并保留 nil 守卫；`internal/ledger/credit.go` 的 `DiffCredits` 跳过 `InfoOnly`（monkeycode「每日 Token 额度」单位是 token，参与差分会产出假 expire），并用本次 InfoOnly 的 key 清掉旧快照误存条目；前端 `renderExpiry` 表头改「额度 · 今日花费」，`spent` 优先 `today_spent`（老后端退回 `today_out`），新增「今日已废」，预测文案「预计作废」改「到期前还会废」并带批次限定词；`scripts/ui-static-check.mjs` 夹具扩成 3 行、闸门 5 条。不移植上游 Urgency/HoursLeft 配色重构（我们结构较旧，属另一功能）。闸门：`internal/stats/r97_netspent_test.go`（5 例，含 1h 观测外推 4980）、`internal/ledger/r97_infoonly_test.go`（2 例）、`views_test.go` 12 处调用同步、UI 静态检查 5 条；六处变异全红；38 包全绿。 |
| R98 | raccoon 签到必须由 `setting_info` 触发，不能用只读端点冒充：R61 用真实浏览器取证时把「打开工作台」触发的懒发放错记到 `GET /api/web/points/v1/balance` 头上，导致 2026-10-29 起用户不再开网页后，每日两次签到共 20 条全部 `msg=上游今日每日积分尚未发放（当日窗口内可重试）` 空转，300 分以 `daily_makeup` 当日即清零补发、余额长期不涨。上游 `internal/raccoon/checkin.go`（fork 点之前就存在，diff 视角永远看不见）实测：`GET /api/web/office/v3/setting_info` 的 `data.point_grant_popups[]` 才是发放触发点，`chat/completions` 只扣 -2、`balance`/`bills`/`user_info`/`iam/confirm` 全部只读不入账，同日重复幂等。改法：新增 `EpOfficeSettingInfo` + `FetchSettingInfo`（走 `doOfficial`，补 R91 的官方指纹头并按账号分桶会话），`DailyCheckinReport` 以 popups 累加 `granted>0` 判 claimed、popups 空则用 bills 交叉核对判 already / `NoCampaign`（反静默空转闸门）、bills 失败保守判 already，所有分支保留 `raccoon checkin setting_info uid=...` 日志形状；`do()` 拆成共享 `doWithHeaders`，普通调用维持精简三头、仅 web 端点走官方套件。八处变异全红。 |

## 2. 架构选型（依据）

| 主题 | 选型 | 理由 |
|------|------|------|
| GUI 壳 | **无**（删除 wails） | WebView2 内存开销大 + Windows 绑定；托盘 + 浏览器足够 |
| 托盘 | energye/systray v1.0.3（现有） | 已跨平台；菜单固定方案规避其不可删菜单项限制 |
| 管理后端 | 现有 http.Server 扩展 /api/* | 单端口、复用鉴权中间件（cookie 会话守卫）、零新服务 |
| Web UI | 纯静态 embed + fetch | 无 Node 构建链，单 exe 双击即用 |
| 登录 | 复用 internal/login + login_trae | 纯 HTTP + 本地回调端口，跨平台 |
| 平台能力 | internal/platform + build tag（windows/darwin/other） | 浏览器无痕/开机自启/消息框/日志/工作区，接口同名 |
| 构建 | WSL 交叉编译 win；CI macos-latest 编 darwin | 见 R11 |

## 3. 托盘菜单设计（当前形态）

```
omnigate
──────────
打开主界面          → 系统浏览器打开 http://<listen>/
查看日志            → 系统默认编辑器打开 data/app.log
──────────
退出                → 退出 daemon（确认框）
```

- 单击/双击/右击：右击弹菜单；**单击与双击 = 打开主界面**
- 各菜单项使用不同颜色纯 Go 生成图标（蓝色=打开、灰色=日志、红色=退出）
- 刷新积分功能已移至 Web UI 面板操作

## 4. Web UI 页面规划（纯静态，一个 index.html + app.js + style.css）

| 页面/区块 | 内容 |
|-----------|------|
| 顶部栏 | 品牌名/版本号、API 地址（点击弹窗配置）、API-Key（点击弹窗修改）、帮助/关于 |
| 登录层 | 启用管理密码时先登录（HttpOnly cookie 会话）；设置弹层内置「退出登录」 |
| 账号管理 | 双列卡片网格，账号名/UID/积分/签到状态，图标按钮操作（签到/刷新/停用/删除）；顶部积分汇总条 chip 可点击筛选（PR #68） |
| 自动签到 | 签到时间（HH:MM 多组）+ 开机自启开关（左右布局） |
| 用量与流水 | token 与积分流水（ledger 口径，R17）；token / 积分两个子 tab |
| 费率 | 各渠道模型定价表（按渠道分组），渠道标签带余额角标、模型名点击复制（PR #69） |
| 运行统计 | 实时统计（stats 口径，R43）：今日情况 / 平台概览 / 使用中接棒 / 最近临期 / 模型消耗 / 请求日志 / 异常账号 / 运行日志，30s 轮询 |

管理 API（REST，均挂 `/api/*`；除 `/api/auth/*` 外均需有效面板会话——密码为空时不校验）：

```
POST /api/auth/login               # {password} → {session}；下发 HttpOnly cookie
POST /api/auth/logout              # 注销当前会话
GET  /api/auth/state               # 会话探针（不返回 401）：全量状态 + auth_enabled/auth_session/auth_required
GET  /api/state                    # 全量状态（账号/积分/签到/配置）
POST /api/login/start              # {channel} → {auth_url}
POST /api/login/glm_auto           # 智谱清言自动登录：拉起独立 profile 浏览器（见 R27）
GET  /api/login/glm_auto_status    # → {status: idle|pending|success|failed|cancelled, uid?, nickname?, error?}
POST /api/login/glm_auto_cancel    # 取消自动登录并关闭浏览器
POST /api/login/glm_token          # {refresh_token} → {uid} 智谱清言手工兜底（见 R22）
POST /api/login/cancel
POST /api/account/checkin          # {uid}
POST /api/account/refresh          # {uid}
POST /api/account/remove           # {uid}
POST /api/account/disable          # {uid,disabled} 停用/启用
POST /api/account/resource_detail  # {uid} → 积分明细
POST /api/account/nickname          # {uid,nickname} 修改显示名
POST /api/account/tasks             # {uid,include_mp} 成长任务「一键完成」（异步受理，R74 前的作业模型）
GET  /api/account/tasks_status       # 成长任务进度
POST /api/account/growth_travel      # {uid} 猫猫旅行手动执行（同步回摘要，R76）
POST /api/account/refresh_all        # 积分刷新批量作业（立刻受理，进度见下，R74）
GET  /api/account/refresh_all_status  # 积分刷新进度 {running,total,done,ok,failed,current}
POST /api/account/checkin_all        # 批量签到作业（立刻受理，R74）
GET  /api/account/checkin_all_status  # 批量签到进度 + 每条结果
GET  /api/inuse                       # 「调用中」徽标轮询专用轻端点（R73，2s 一次，别用 /api/state）
POST /api/config/checkin_times     # {times:["09:00","21:30"]}
POST /api/config/listen            # {host,port,admin_password?}（非环回时必须带密码）
POST /api/config/admin_password    # {password}（空 = 关闭面板鉴权，仅环回监听允许）
POST /api/config/api_key           # {key}
POST /api/config/autostart         # {on:bool}
POST /api/config/compat            # 模型路由（默认渠道 / 封顶 tokens / 映射表）
POST /api/config/expiring_days     # {days} 临期阈值
POST /api/config/proxies           # {proxies:{channel:url}} 单渠道上游代理
GET  /api/fees                     # 渠道费率（本地缓存 + 按需刷新）
POST /api/fees/refresh             # 异步刷新费率
GET  /api/usage                    # 用量/积分流水聚合（R17，ledger 口径）
GET  /api/stats                    # 运行统计快照（R43，stats 口径）
GET  /api/stats/logs?limit=15      # 运行统计的最近请求日志行
GET  /api/logs                     # 最近 300 行日志
POST /api/quit                     # 退出程序
```

## 5. 渠道（已实现 WorkBuddyCN + WorkBuddyAI 国际版 + TraeWork + TraeCode + QoderCN + QoderCOM 国际版 + 千问办公 + MonkeyCode + 小浣熊 + Loomy + 智谱清言；旧 Qoder 已下线）

1. 新建 `internal/<channel>/` 包，实现 `provider.Upstream` 接口
2. `internal/auth` 增加对应 `Load<Channel>Dir()`（文件名前缀 `<channel>-*.json`；
   **glob 边界**：`qoder*.json` 会吞掉 `qodercn-`/`qodercom-` 前缀，LoadQoderDir 必须显式排除）
3. 装配处注册 `server.Runtime{Kind, Pool, Upstream, StaticModels}` + `app.Runtime{..., Scheduler}`
4. 前端渠道选择器加一项；`internal/login_<channel>` 实现登录编排（如需）
   > **无账号渠道跳过第 2、4 步**：不建 auth 文件与加载器，虚拟账号由 `main` 装配时注入 pool，
   > 且 `app.reloadAccounts` 不得纳入（否则 `SyncToDir` 会把它剔除）。

> provider.Kind 即模型名前缀；server 按 `channel/<model>` 前缀路由，无需改接口。
> **QoderCN（`qodercn/*`）**：qoder2api 参数形态（cosyVersion 1.0.10、18 头含 cosy-scene 族、
> session_type=qoder、identity userType 实测回填）；签到仅 campaigns（legacy daily-check-in 已 DISABLED，
> 其 claim 恒返回 409 会造成假成功，故不再使用）；每日 10:00 开放 + 10:00–12:00 窗口内重试（见不变量 23）；
> 动态模型表（无静态兑底，上次成功缓存）。详见 `internal/qodercn/`。
> **QoderCOM（`qodercom/*`）**：国际版（qoder.com/openapi.qoder.sh/api1+api2.qoder.sh 三域分离）；
> 凭据与 CN 区完全隔离（双向 401）；签到仅 campaigns（无 daily-check-in，实测 404）；
> 每日 10:00 开放 + 10:00–12:00 窗口内重试（同 CN，见不变量 23）。详见 `internal/qodercom/`。
> **旧 Qoder（`qoder/*`，QoderWork）已从界面下线**：代码与路由保留，存量账号仍可用；不新增功能，后续可移除。
> WorkBuddyAI 国际版：`DailyCheckin` 实现为「免费模型对话保活 + 签到探测」（对用户透明，无前端界面）；
> token 有效期 365 天，故 KeepaliveHours 设为 nil。详见 `internal/workbuddyai/`。
> **MonkeyCode（`monkeycode/*`，平台托管模型）**：凭据由面板「从本机客户端导入」从官方客户端
> （ohmyagent）的 settings.json 取得，一个账号 = `oma_` api_key + `omas_` signing_secret；
> 按模型声明的 type 分流到 `{base}/messages`（Anthropic 形状）或 `{base}/responses`（Responses 形状），
> 两路都是无状态 SSE；上游无目录/额度/刷新接口 → 模型表静态、额度恒 0、`RefreshToken` 空实现。
> 协议对照参考：`codkeep/MonkeyCodeReverseEngineer`（Apache-2.0）。
> **小浣熊（`raccoon/*`）**：两条添加路径 —— 面板登录按钮走浏览器授权（登录期间临时把
> `office-raccoon://` 协议回调指向本工具以接住授权码，结束立即恢复注册表，见 internal/login_raccoon），
> 弹窗次按钮「从客户端导入」读本机 `.box-agent/config/auth.json`。凭据 access_token ≈2h +
> refresh_token ≈30d，保活每 4 小时一次以减少「请求先 401 再刷新」的往返；无签到端点。
> 上游默认档即最深思考，故不接档位面。
> 接口结构整理：`xxhhlk/raccoon2api`（MIT）。
> **Loomy（`loomy/*`）**：导入型渠道 —— 凭据由面板「从本机客户端导入」读
> `C:\Users\Public\Loomy\<hash>\userData\auth-session.json`。上游**无 refresh 端点**，
> 客户端声称 session 约 14 天，但**实测过期后仍可用**（某号 `expiresAt` 已过期 3 天，
> `POST /api/account/resource_detail` 仍返 200），故到期时刻写**远期哨兵**（R94），真实失效
> 只能靠 401 / 业务码兜底；`RefreshToken` 留空、定时任务全关（无签到任务 ⇒ 不再每天落一条失败）。鉴权失败是 **HTTP 200 +
> 业务码 `100002`**（不是 401），业务码判定必须排在状态码判定之前。
> 接口结构整理：`xxhhlk/loomy2api`（MIT）。
> **智谱清言（`glm/*`）**：网页版私有接口（**非**开放平台 open.bigmodel.cn），凭据是浏览器 Cookie 里的
> `chatglm_refresh_token`；所有私有接口需签名 `X-Sign = md5(ts-nonce-secret)`；模型 = `assistant_id`
> （24 位 hex 智能体 ID）+ `chat_mode`（`zero` 推理 / `deep_research` 沉思 / `ppt` / `video`）。
> **模型清单是「智能体清单」不是「模型版本清单」**——清言客户端无法选模型版本，
> 服务端按账号分配（实测主对话恒为 `moe_53f`）；GLM-5.3 的具体版本控制只能走官方开放平台。
> 静态表**只收录实测可用的 4 个智能体**（ChatGLM / AI搜索 / 清言PPT / 视频助手）；
> AI画图（需 cogview 参数）、AI阅读（需上传文件）、学习搭子（上游 10025 报错）**不收录**——
> 想用未收录的智能体直接填其 24 位 hex ID。
> **积分机制**：`member_info.score_rule` = 「免费用户，登录赠送200积分/天」。
> **新账号激活**：加进来后 `left_score=0`，**必须在智谱清言 App 登录一次**才发放（+3000 随后 +500）——
> 对照实验证实（R23）。**代码层面无解**（Web 端无激活接口，R32 实测），
> 故额度为 0 的账号会永远排最后、实际轮不到。
> **每日积分**：走 `member-api/member/daily_login_score`（**已接入保活流程**，R31）——
> 该接口网页版打开时自己就会调，幂等安全（`status=10001 今日已领取` 是正常语义）。
> 「签到」的实质是**保活对话**。**操作建议**：面板加完账号 → 手机 App 登录同账号一次 → 之后每天自动领。
> SSE 为**增量 delta**（`finish` 帧给全文，见 R25）。登录走 CDP 自动捕获 Cookie（R27/R28），
> 支持多账号（独立 profile 逐个添加）。详见 `internal/glm/`（含 CDP 登录编排 `internal/login_glm/`）。

## 6. 关键不变量（改动前必读）

0. **每次代码变更后必须本地重新构建 `dist/omnigate.exe`**（见 §8）。
   `dist/` 在 `.gitignore` 中，CI 只产出带平台后缀的 `omnigate-<os>-<arch>`，
   **不会**生成 `dist/omnigate.exe`——该文件只能手动构建。
   不重建会导致：本地运行的二进制与源码不一致（例如改了版本号但仍显示旧版本）。
1. `PrepareBody` 三改写勿动：强制 `stream=true`、`tool_choice` 归一化、`developer→system`
2. 日志/面板/消息框**零 token**：不得输出 access/refresh token（调试用假 token）
3. auth 文件嵌套格式 `{auth:{...},account:{...}}`，`internal/auth.Parse` 与 login.SaveAuth 必须一致
4. `config.listen` 兼容新对象格式 + 旧字符串格式 `":7863"`
5. `data/state-*.json` 只增不减字段，向后兼容；旧 state.json 自动迁移
6. 托盘回调必须 goroutine 化
7. `config.example.json` 与 `config.Default()` 同步
8. 上游 HTTP ≥400 错误直接透传原始响应，不包装
9. 定价缓存持久化到 `data/pricing-cache.json`，启动加载，超 1h 自动刷新
10. 无桌面 Linux 必须 `--no-tray`，不带参数 panic 直接 exit 提示
11. **粘性路由**：`pickWithSticky` 优先复用上次账号，直至连续成功请求达 50 次或遭遇错误冷却。成功时 `stickySuccess` 递增计数，错误时 `stickyClear` 清除粘性记录。不使用 credits 阈值（pool 中余额是 stale 数据）。
12. **`internal/server` 主链路不得被绕过**：`POST /v1/chat/completions` 与 `GET /v1/models` 由内层直接服务，`internal/gateway` 只接管 `/v1/responses`、`/v1/messages`、`/v1/messages/count_tokens`。
13. **兼容层调用内层只能经 `Gateway.call()`**（`io.Pipe`），调用方读完必须 `res.Close()`，否则内层 goroutine 可能阻塞在 Write 上泄漏。
14. **`pipeRW.Flush()` 为空操作是刻意的**：`io.Pipe` 无缓冲，Write 即送达；不要改成缓冲 + 定时 flush。
15. **错误分类 429 必须优先于 hardMarkers**：限流 body 高频带 `quota exceeded`，先判 hardRule 会把限流误归余额耗尽 → 12h 硬冷却。三渠道 `Classify` 均已修复此顺序。
16. **脱敏层仅做文本替换不做语义变更**：`internal/sanitize` 只改模板句、不改用户内容语义；预检不命中时零分配原样通过。将来配置 `features.sanitize_fingerprints` 可一键关闭（逃生门）。

17. **本机的读取通道会失真，只有 go test 的 `% x` 打印可信** —— 改脱敏表前必读。
   在本机用 `sed` / grep / python `print` / edit 工具读取含特定指纹字面量的源码时，
   **输出会被静默截断或转写**，且不同通道给出的结果互相矛盾。已实测三例：
   - `reHdr` 显示为 `(?i)\n]*;?\s*`（正则主体丢失），运行时其实是完整的
     `(?i)x-anthropic-billing-hdr\s*:[^;\n]*;?\s*`；
   - `sanitize_test.go` 的 `dshIdentity` 常量显示为 `"."`，运行时其实是
     完整的 48 字节身份句；
   - 脱敏表的 old/new 两值曾被渲染成完全相同的字符串，看起来「全是空转」。

   **后果**：照着失真输出改代码会把真实有效的改写删掉，或把替换源写成空串
   （`strings.ReplaceAll(text, "", X)` 会往每个字节间隙插 X，属真实破坏）。
   这条教训 R82 期间踩了 6 次。

   **唯一可信的取证方式**：在 `go test` 里打印字节。
   ```go
   fmt.Printf("PROBE x = % x\n", []byte(x))
   ```
   动手改动脱敏表（两个 `sanitize.go`）之前，**先跑一遍 `zz_probe_test.go`
   打印全部涉及的字面量**，确认磁盘上的真值，再改；改完再跑一次核对。

   **写代码的纪律**（三条都由这条不变量推出）：
   - 不要用 `edit` 的 old/new 匹配去改这些文件（渲染态与磁盘字节不一致，
     匹配会全失败或改错位置），改用 python 按行号/按字节替换；
   - 新增的特征串、正则若要包含会被转写的字面量，用**运行时拼接**
     （如 `looseHdrKeyExpr()` 的单字母片段）而不是正则字面量；
   - 测试用例的输入也用 `hex` / `[]byte{...}` 构造，别写字面量。
18. **积分「可用/不可用」拆分统计**：`provider.ResourceItem.Usable` 标记条目是否属于本工具可消耗的额度池，`provider.Summarize()` 汇总小计。
    - TraeWork 判据（2026-09-26 修订，R19 / issue #44）是 **`available_endpoint==1` 为不可用**
      （pid==209 判据已于 09-26 撤回，见下）：
      上游已不再下发 ep=1（专用池也标 0），ep 判据仅作历史兑底。**pid==209 判据已于 2026-09-26
      撤回**（issue #44）：pid=209「200 档每日签到」升级为通用积分，实测扣费就发生在这个池上
      （09-23 99 次对话期间 remain 恒 3086、「不可用」小计 -354），继续排除会让 pool 按虚低余额选号、
      面板把真实可用积分误标「不可用」。**不得用 `group_type` 判定**——同名「每日签到」既有
      通用份也有专用份。早期仅用 ep 判定的实测证据见 `internal/upstream/` §2.3。
    - `UserResource` / `UserResourceDetail` 返回的 remain **只能是可消耗余额**，
      否则 pool 会按虚高余额选号。含专用池的总量（`usage_summary.total_amount`）不能作路由依据。
    - 不可消耗额度仅用于面板展示（`pool.Status.UnusableCredits`），不参与 `Pick()` 排序；
      展示的唯一目的是让用户看到的总积分能和官网对上。
19. **到期时间字段因渠道而异，缺失则不显示**：WorkBuddy 系是 `CycleEndTime`（**上游从不下发 `PackageEndTime`**，旧判据恒 miss），
    TraeWork 是 `expire_time`（Unix 秒），Qoder 无此字段。均按 **UTC+8 墙钟**解析（`softRateResetLoc`），
    用 `time.Local` 会在非 UTC+8 机器上算错一天。上游未下发时 `ResourceItem.ExpireAt` 必须为空串，
    前端据此隐藏整列——**不得用零值时间冒充「永不过期」**。
20. **401 必须自愈，不能只信本地 `expiresAt`**：上游刷新会作废旧 access token（refresh token 同步轮换）。若新 token 未落盘、
    或同一账号在别处被刷新，本地文件里的 token `expiresAt` 仍在未来，但上游已拒绝 → `NeedsRefresh` 恒为假、永不刷新、
    积分恒 0、明细恒空。因此积分/明细/费率路径遇 `ErrSessionDead` 必须「refresh + 落盘 + 重试一次」
    （`app.refreshIfSessionDead`，scheduler 的 checkin 路径同理）。
21. **凡是调 `Upstream.RefreshToken` 的地方必须紧跟 `SaveAtomic`**：refresh token 会轮换，不落盘 = 下次启动用旧 refresh token，
    重回上一条的死锁（`RefreshPricing` 曾漏，已补）。
22. **虚拟账号/单账号渠道不得进入任何「能把它弄没」的路径**：`reloadAccounts` 不纳入（`SyncToDir` 会剔除），
    启动时 `SetDisabled(uid,false)` 兜底自愈；`RemoveAccount`/`DisableAccount` 对此类账号硬拒（后端拒 + 前端无入口）。
    其 `Auth.ExpiresAt` 必须为远期值（不得为 0），否则 `NeedsRefresh` 恒真 → 反复 `RefreshToken` + 冷却。
    > 历史上唯一的无账号渠道是 OpenCodeZen（`oczen/*`），已于本项目移除。
    > 该渠道留下的两处机制仍在代码里，将来接入新的匿名通道可直接复用：
    > `Runtime.SingleAccount`（下条）与 `NO_CREDITS` 空集（前端「积分不适用」显示）。
23. **单账号渠道（`Runtime.SingleAccount`）不得施加任何账号级惩罚**：唯一账号且不可重登 ⇒ 任何惩罚
    （冷却/计数/禁用）都等于整条渠道下线。故 `Runtime.SingleAccount=true` 的渠道在 handler 里
    **传输层错误与 `status>=400` 一律原文透传**，不走 `NoteError`/`Cooldown`/`Disable`。
    - **429 也不冷却**：无号可轮换，冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，
      不如透传 429 让客户端按 `Retry-After` 退避。
    - **传输层错误不累计 `errCount`**：否则 3 次网络抖动（默认 `ErrThreshold`）即冷却唯一账号。
    - 启动时 `Pool.ClearPenalty(uid)` 自愈遗留冷却（`SetDisabled` 只清禁用，不清冷却）。
    - **严禁**把 401/403 归为 `ErrSessionDead`（会 `pool.Disable` 永久禁用且无法人工恢复）。
    - 该渠道的 `Classify` 仍做语义分类（供日志），但**不再用于决定惩罚**（惩罚判定前已短路）。
24. **Qoder 双区签到只能走 campaigns，且状态必须结构化上报**：
    - **绝不调用 legacy `daily-check-in/claim`**：该端点已全局 DISABLED，却对未领取日恒返回 409，
      会被误判成「今日已领取」而跳过真实领取 → 假成功、零积分（上游 `99ab022` 同款结论，2026-09-21 抓包实测）。
      只有 `GET /sash/api/v1/me/campaigns` → `POST .../campaigns/{id}/claim` 会真实发放 100 Credits。
    - **401 必须返回 `*provider.Error{Kind: ErrSessionDead}`**，不得用裸 `fmt.Errorf`：调度器
      `isSessionDead` 走 `errors.As` 类型断言，裸 error 会让它恒为 false，自愈失效（不变式 19）。
    - **结果状态经 `provider.CheckinReporter` 上报**（`CheckinClaimed`/`Already`/`NoCampaign`/`NoToken`/`Error`）：
      `error` 通道无法区分 `no_campaign` 与 `error`——两者都不是「已签到」，却都需在窗口内重试。
      **`Msg` 必须透出**，不得被调度器覆盖成 `"ok"`，否则面板无法区分「真领到 100」与「活动还没上线」。
    - **签到窗口必须带重试**：每日 10:00（UTC+8）开放，活动可能在整点后才创建，
      故 `CheckinMinutes=[10:00]` + `CheckinRetryUntil=12:00`，窗口内每分钟重试，
      直到全部账号达 `claimed`/`already` 才算当日完成（上游 `ae3d42f` 修的就是漏领一天）。
      完成标记按「日期+时段」而非仅日期——本工具支持一天多时段（09:00/21:00），
      只按日期标记会让早间成功吞掉晚间时段。

25. **不许把两份脱敏表「合并真源」，除非先做对照实验**：CN 走两段式（`internal/upstream` 表 → `internal/sanitize` 表），
    stage1 吃掉指纹后 stage2 恒为 no-op ⇒ **CN 出站的实际改写形态由 `internal/upstream` 表决定**；
    `internal/workbuddyai` 的 `prepareBodyInner` 没有 stage1 ⇒ 国际版由 `internal/sanitize` 表单独生效。
    两份表的**匹配键（old）已对齐**（R84/R85），但**替换目标（new）形态不同**：upstream 用等价词，sanitize 用 leetspeak。
    任何「并成一张表」的改动都会改变 CN 出站字节，而 CN 只在等价词形态下有生产背书
    （2026-10-27 之后 10529 次请求零指纹拒信）。要改必须按不变式 R23b 做**对照实验**
    （同一请求两种形态各发一次上游，看是否被拦），不能凭架构整洁直接切。
    闸门：`internal/upstream/r87_two_stage_test.go`（删 stage1 立刻红）。

26. **本地判定的请求错误不得用 404，且必须落进不可轮转的 kind**：404 的语义是「上游说没这个资源」，而 `provider.ErrKind.Rotatable()` 把 `ErrNotFound` 当账号级错误（换号有价值）。本端校验（模型不在本地目录、参数越界、请求形态不接受）用 404 会产生**换号必然复现**的死循环，且每次轮换都 `NoteError` 一笔——客户端打错一个名字就能把整条渠道冷却。正确做法两件同时做：①状态码用 400/4xx 表示请求错误；②响应体带专属文案让 `Classify` 归到 `ErrBadParams`/`ErrPassthrough`/`ErrModelBlocked`（三者都不在轮转白名单）。只改状态码不够：`ErrClient` 同样可轮换。参照实现 `internal/loomy/client.go`，事故案例 `internal/cline/client.go`（R88）。

27. **能力声明只许来自上游声明，nil ≠ false，单档不算选择**：`/v1/models` 是客户端渲染「思考等级 / 上下文大小」下拉框的**唯一**数据源，缺字段就是用户「没得选」。三条纪律：
    - **不编造**：`ReasoningEfforts` 空 = 上游没给档位表（典型：qoder 系只声明 `is_reasoning` 一个 bool，档位是透传给上游的字符串），此时**不得**补 `low/medium/high`；给 qoder 补档就是替上游做主，客户端选了会拿到上游未定义的行为。
    - **`nil ≠ false`**：`CanDisableThinking *bool` 为 nil 表示未声明、不输出；为 `false` 表示上游明确说「思考不可关」，**必须输出 false**——两者混同会让客户端把不可关的模型当可关。
    - **单档不算选择**：`ContextWindows` 只在上游给出 ≥2 档（qoder 系 `context_config`）时输出 `context_windows`；单档仍以 `context_length` 声明事实。
    另：`ContextWindows` 必须**拷贝**上游切片（`append([]int64(nil), ...)`），共享底层数组会让后续写入反向污染已缓存模型表。
    闸门：`internal/server/r89_capability_declare_test.go`、`internal/{qoder,qodercn,qodercom}/r89_context_windows_test.go`、`internal/loomy/r89_reasoning_efforts_test.go`（五处变异全红）。

28. **渠道指纹与会话头按上游真实语义生成，不按「能通过校验」生成**：R44 只证明了「头必须齐」，从头约束过「取值语义」。两类语义一旦错，后果不是 400 而是**悄悄被限流**。
    - **UA 与 SDK 头照抄原文**：raccoon 的 `User-Agent` 必须是 `AsyncOpenAI/Python 2.8.0`（openai SDK 的 `BaseClient.user_agent` 原文）；`X-Stainless-Arch` 在 Windows amd64 上是 **`other:amd64`**（SDK 的 `get_architecture()` 对未知 machine 走 `OtherArch`，`__str__` 为 `other:<machine>`），只有 Linux/macOS 的 `x86_64` 才是 `x64`。不要凭直觉填 `amd64` 或 `x64`。
    - **不手写 `Accept`**：`Accept: application/json` 由 SDK 的 `default_headers` 自带；流式请求也**不**改成 `text/event-stream`。
    - **空值不发，宁缺不编造**：`cleanHeaderValue` 规则是 TrimSpace 后空 / 长度 >256 / 含非 ASCII（r>127）⇒ 该头整个不发。取不到系统版本就发空串，不要编一个。
    - **会话 ID 必须复用，不许每请求新建**：raccoon 24h 池（`sessionKeyOf` 必须含账号）；convheaders 按锚点派生（同 uid + 同锚点 = 同 id）。每请求一个新会话是上游 429 的直接触发器。
    - **两份 convheaders 必须同步改**：`internal/upstream/` 与 `internal/workbuddyai/` 是平行实现（同 R52 脱敏表纪律），不许抽成共享包（渠道包之间不共享实现，R58/R111）。
    - 闸门：`internal/raccoon/r91_headers_test.go`、`internal/{upstream,workbuddyai}/r91_{convheaders,wiring}_test.go`。

29. **传输层瞬态错误必须同号原地重试，且只在最终失败时留证**：这台机器上每个渠道都只有 1 个账号（`data/state-*.json` 的 `accounts` 恒为 1），所以 DNS 抖动、拨号超时、连接被掐断这类**秒级自愈**的故障换号救不了，只能在同号上重试。四条纪律：
    - **只重试传输层**：`do` 返回 err 才重试；HTTP 4xx/5xx 的语义由各渠道 `Classify` 决定（5xx 重试会把「上游真挂了」变成三倍流量）。
    - **判据必须先拆 `*url.Error`**：`url.Error` 自己实现 `net.Error`，不拆的话 `invalid header field value` 这种**本地**错误会被误判成瞬态，白重试三次。
    - **重放靠 `GetBody`+`Clone`，不可重放就只发一次**：发空 body 比一次失败更糟（上游会记一笔无效请求）。无 body 的请求（GET）`GetBody` 是 nil 但完全可以重放，不能拿它当唯一判据。
    - **只在最终失败落证**：`capture.Save` 有 `maxFiles=500`/`maxBytes=32MB` 上限，中间失败也落证会让一次抖动产生 3 份重复记录，把真实证据挤出去。
    - 失败时必须返回 `noopCancel`，**不能返回 nil**：各渠道错误分支都无条件调 `cancel()`（成功时那是 `idle.WithCancel` 的句柄）。
    闸门：`internal/httpretry/httpretry_test.go`、`internal/traework/r92_retry_test.go`（五处变异全红）。

30. **流式必须有「首内容截止」，且日额度耗尽不得当成请求错误**：R93 的两条硬约束。
	①`FirstContentTimeout`（缺省 90s）是**首帧非空内容**的截止，不是首字节截止——`role` 首帧、空 `content`、空 `tool_calls` 都不算内容。上游发完首帧就沉默是 Qoder 系额度耗尽的典型形态，「继续等」只会把 180 秒的锅甩给用户。
	②到点必须走 `firstContentReader.poison()` 而不是裸 `Close()`：Close 让读取方拿到底层错误串（`io.ErrClosedPipe` / `use of closed network connection`），R90 的 `TruncationCode` 就会把它归成 `upstream_truncated`，首内容超时与上游真断又同形了。
	③`403` / `402` 上的额度文案必须归 `ErrHardCredit`。`ErrClient` 在 `Rotatable()` 白名单里，而本机每渠道只有 1 个账号 ⇒ 换号必败，唯一结果是「多记一笔账号错误 + 503」。新增额度文案时**8 份 hardMarkers 要同步改**，`internal/provider/r93_quota_markers_test.go` 会在漏改时转红。

31. **签到时刻的归属与签到状态必须由渠道语义决定，不能由全局设置决定**：R94 的三条硬约束。
	①「面板上的签到时间」只是**全局默认**，不是所有权。Qoder 双区固定在 10:00 开放、MiniMax 在 00:00 重置积分，这些时刻是上游语义；被用户随便改的时间覆盖就当天漏领，且没有任何报错。新增渠道时：用全局时刻的加 `FollowGlobalCheckin: true`，固定窗口或无签到的**不要加**——`internal/app/r94_main_wiring_test.go` 会按白名单双向校验。
	②「这个渠道没有签到任务」必须显式表达为 `CheckinMinutes: []int{}`，且 `finishCheckin` 在这种渠道上**不落盘也不上报**。否则调度器每天空跑一次、面板每天多一条失败，用户会以为渠道坏了。
	③无 refresh 端点的渠道（loomy / monkeycode）到期时刻一律写远期哨兵，**不要**按「客户端声称的有效期」估算。`NeedsRefreshLocked` 只看 ExpiresAt，写短期值等于给该渠道定时埋一个恒真刷新失败；真实失效只能靠 401 兜底。
32. **脱敏只碰提示词角色，且不得整包改写**：R95 的三条硬约束。
    ① `system`/`developer`/`assistant` 要洗，`user`/`tool`/`function` 一个字节都不许动（用户正常提到这些词是合法内容）；
    ② 禁止整包正则替换（它会篡改 `tools`/`metadata` 等调用方字段且不可逆）；要覆盖新形态就加**定向**规则；
    ③ 预检与替换必须对同一批变体敏感（预检用 fold、替换用区分大小写的 `ReplaceAll` 就是漏洞）。
33. **「倍率 0」有两种，显式 x0.00 是定价信息不许丢**：R96 的硬约束。
	① 上游 `credits` 字段缺失 = 不计费的内部模型（hunyuan-chat 等），跳过；`credits` 显式为 `"x0.00 credits"` = **免费**（限时免费促销），必须保留且 `Explicit=true`。按 `rate<=0` 一刀切会把免费模型从费率表、`/v1/models` 的 `(0.00x)` 后缀、面板「✦ Free」里全部抹掉，用户只能去官网才知道哪个模型免费。
	② 「面板隐藏无倍率模型」只对**真的没有倍率**的模型成立；一个模型「未定价」和「定价为 0」在下游必须能区分（`provider.ModelPricing.IsExplicit()` + `feesModelRow.Free`）。
34. **「到期作废」不是「消耗速度」，净花费必须扣作废且只在 uid 维度拆**：R97 的硬约束。①`today_spent = max(0, creditOut − expiredByUID[uid])`，`burnForecast` 必须用净花费外推，净花费≤0 不外推；②`expiredByUID` 与 `expiredByGroup` 同源但不同维度，全量替换、跨天清零、空 UID 只进渠道维度；③`InfoOnly` 纯展示条目量纲可能不是「分」，严禁参与 `DiffCredits` 差分，旧快照残留要用本次 InfoOnly key 清掉；④前端「今日花费」/「今日已废」/「到期前还会废」三数口径不同不必相加相等，老后端无 `today_spent` 退回差值，预测负数要按全部额度兜底。
35. **「懒发放」类签到必须先打触发端点、再读判据，只读端点永远不能算签到**：R98 的硬约束。①判据必须是「触发端点返回的发放凭证」（raccoon = `point_grant_popups[]`），不是「余额/流水里碰巧有一条发放记录」——后者是观察结果，观察本身不产生发放；②触发端点必须带与官方客户端一致的指纹头（R91 `ApplyHeaders`），精简三头可能被判定为非官方调用而不发放；③「触发了但上游今日没发」必须报可重试状态（`NoCampaign`），严禁谎报 already 造成静默空转；④流水只作交叉核对/兜底，核对失败保守判 already 并带「核对失败」字样，不能升级成 error 让调度器无谓重试。

## 7. 平台能力差异表（internal/platform）

| 能力 | Windows | macOS | Linux |
|------|---------|-------|-------|
| 打开浏览器 | rundll32 url.dll | `open <url>` | xdg-open |
| 系统消息框 | MessageBoxW | osascript display dialog | stderr |
| 开机自启 | 注册表 Run | LaunchAgent plist | 未实现 |
| 打开日志文件 | notepad | open -a TextEdit | xdg-open |
| 确认框 | MessageBoxW YESNO | osascript buttons | 默认否 |
| 无头模式 | --no-tray | --no-tray | --no-tray（推荐） |

## 8. 构建

> ⚠️ **本地构建是日常约束**：任何代码变更后都要重新构建 `dist/omnigate.exe`
> （见 §6 第 0 条）。CI 不生成该文件，且 `dist/` 不入版本控制。
> 标准流程：`go build ./... && go vet ./... && go test ./...` 全绿后再构建。

```bash
# Windows（本机直接构建，或 WSL 交叉编译）
bash scripts/build_release.sh          # 发布用构建（含 -trimpath -s -w + 产物自检）

# macOS（需 macOS 真机或 CI，cgo 必需）
GOOS=darwin GOARCH=arm64 go build -o dist/omnigate-darwin ./cmd/omnigate

# Linux 无头
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/omnigate-linux ./cmd/omnigate
```

构建后核对版本号已进二进制（防止拿到旧文件）：

```bash
# Windows bash 下用 python 字节计数（strings 对 Go 二进制的长串不可靠）
python -c "b=open('dist/omnigate.exe','rb').read(); print('new:',b.count(b'2.5.3'),'old:',b.count(b'2.5.2'))"
# 期望：new >= 1 且 old == 0。若旧版本号仍在，说明构建未生效。
```

### 发版（tag 触发）

push `vX.Y.Z` tag → GitHub Actions 构建五平台产物并创建正式 release。

> **触发与守卫**：`on.push.tags: 'v*'` 只是粗筛（`version` 这类标签也会命中），真正的校验在 `guard` job：
> 必须是 `vX.Y.Z` 或 `vX.Y.Z-预发布`，否则在 build 之前就拒（R77）。
> 发布步骤**幂等**：同名 release 已存在时走 `upload --clobber` + `edit`，不会因「already exists」失败。
> 手动 `workflow_dispatch` 检出的是**默认分支**（`github.event.repository.default_branch`，本仓库 `main`）。

**release note 用仓库根目录的 `RELEASE-<tag>.md`（手写摘要，面向用户）**，
而不是 `--generate-notes`（那只给 commit 链接列表）；文件缺失时回退自动生成，不阻塞发版。

```bash
# 发版前确认：版本常量已 bump（internal/app/app.go const Version）、
# RELEASE-vX.Y.Z.md 已写好且与 tag 名一致、dist/omnigate.exe 已本地重建验证
git tag vX.Y.Z && git push origin vX.Y.Z
```

## 9. 文档索引

入库文档（`docs/` 白名单制下反选入版本控制）：

- [README.md](README.md) — 用户文档
- [DEVELOPMENT.md](DEVELOPMENT.md) — 开发者文档（面向 AI Agent）
- [AGENTS.md](AGENTS.md) — 本文件：决议项一句话判据（R1–R98）、架构选型、不变量
- [docs/DECISIONS.md](docs/DECISIONS.md) — 决议全文（R1–R98）：每条决议的原始背景、踩坑记录与边界条件
- [docs/三接口兼容改造备忘.md](docs/三接口兼容改造备忘.md) — 三接口（Chat/Responses/Anthropic）兼容层架构决策、实施记录、验证清单、已知限制
- [docs/用量积分流水记账备忘.md](docs/用量积分流水记账备忘.md) — 双流水统计（token/积分）架构、差分算法、实测验证、已知限制（R17）

以下备忘被 R16 / 不变量 22 / 不变量 23 等决议引用，但**尚未入库**（`.gitignore` 白名单未反选，仅本地可见）：
`internal/server/handler.go` 的 `Runtime.SingleAccount` 分支、`internal/qwenwork/`、`internal/qodercn/`、
`internal/qodercom/`、`internal/glm/`（含 CDP 登录编排 `internal/login_glm/`）（R22–R38 引用，随 PR #46 新增，
作者未提交入库）。（部分可能已丢失，仅存在于历史会话中）。
如需转为本仓可查，在 `.gitignore` 补 `!docs/<文件名>` 并在上方列表添链接。
**例外**：glm 渠道的协议依据（R22–R38 引用）**不在本地**——作者已将其开源为独立仓库
[glm2api](https://github.com/ttales430/glm2api)（签名算法/认证流程/SSE 语义/积分机制/智能体清单），
引用一律指向该仓库，不要找本地文件。

> **docs/ 采用白名单制**：`.gitignore` 中 `docs/*` 默认忽略全部文档，仅 `!docs/<文件名>` 显式反选的才入库。
> 逆向分析类文档一律**只保留本地、不入库**。新增需要入库的文档时，追加一行 `!docs/<文件名>`。
> 未入库的本地文档（`ref/`、`docs/` 其余文件、`HANDOFF.md`、`GO多平台发布备忘.md` 等）仅供本地参考，
> 不要在本文件中作为可点击链接引用。
