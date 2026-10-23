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

| # | 决议 | 说明 |
|---|------|------|
| R1 | **托盘菜单固定，不做动态内容、不做定时/事件刷新** | 用户在自己客户端操作无法捕捉，动态展示无意义 |
| R2 | ~~托盘提供「刷新积分」菜单~~ **已移除**。刷新积分改为 Web UI 面板操作 | 托盘菜单精简为：打开主界面 / 查看日志 / 退出 |
| R3 | 托盘固定菜单项：**打开主界面 / 查看日志 / 退出** | 双击托盘 = 打开主界面；不再弹"已启动"提示框 |
| R4 | **管理 API 采用 cookie 会话鉴权**（2026-09-24 修订原「不设鉴权」） | 新增 `config.admin_password`（默认缺失 = 不鉴权，向前兼容）；**监听非环回地址时强制要求设置**（启动层 fatal + `SetListen` 拒绝），否则局域网任何设备可无凭据访问面板/退出程序。会话实现见 `internal/app/session.go`：token 随机 + 内存态（只存 SHA-256），HttpOnly + SameSite=Lax cookie，7 天滑动续期，改密码即失效，登录失败 5 次/IP 锁定 5 分钟。`/api/auth/*` 不鉴权，其余 `/api/*` 走守卫；OpenAI 侧 `api_key` 不受影响 |
| R5 | **Web UI 用纯静态 HTML/CSS/JS**（无前端编译链） | `go:embed` 打进单文件；实用 + 大众审美即可 |
| R6 | 托盘库：**保留 energye/systray**（已跨平台 Win/mac/Linux） | 各菜单项使用不同颜色纯 Go 生成图标，无需外部图标文件 |
| R7 | **移除 wails / WebView2 全部依赖** | 省内存与运行时；平台能力封装进 `internal/platform`（build tag 拆分） |
| R8 | daemon 单进程：一个 `http.Server` 同时服务 OpenAI 端点 + 管理 API + 静态 UI | 沿用 server 现有 ServeMux 扩展 |
| R9 | 核心业务（pool/scheduler/upstream/traework/server/login/config/auth/provider）**整体复用**，格式零迁移 | config.json / auths/ / data/state.json 兼容旧版；旧 state.json 自动迁移到 state-workbuddy.json |
| R10 | 新增渠道扩展方式：实现 `provider.Upstream` 接口 + auth 加载器 + 注册 Runtime | 模型前缀 `channel/<model>` 路由；已实现 WorkBuddyCN(国内) + WorkBuddyAI(国际) + TraeWork + TraeCode(与 TraeWork 共账号，function=solo_agent) + QoderCN + QoderCOM(国际) + 千问办公(qwenwork) + MonkeyCode(monkeycode，平台托管模型) + 小浣熊(raccoon) + Loomy(loomy) + 智谱清言(glm) 共十二渠道；旧 Qoder（`qoder/*`，QoderWork）已从界面下线但路由保留 |
| R11 | Windows 产物在 WSL 交叉编译（`GOOS=windows CGO_ENABLED=0`，已验证可行）；macOS 产物走 GitHub Actions macos-latest（cgo 必需） | WSL 无法编 darwin cgo；CI 增加 darwin job |
| R12 | **无桌面 Linux 使用 `--no-tray` 参数** | 无参启动在无 DBus 环境托盘 panic 直接 exit 并提示；`--no-tray` 跳过托盘打印信息阻塞等待 Ctrl+C |
| R13 | **三接口兼容采用两层结构：内层 handler 不动，新增 `internal/gateway` 边缘层**，经 **in-process 调用**（`io.Pipe` + ResponseWriter 形状）复用内层 | 代码量比内联重构多 20%，但改动面小一个数量级（主链路仅 2 处调用点 + 1 个访问器），回归风险低、可脱离 pool 单测。**不得用 HTTP 自环**（`0.0.0.0` 监听不可作目标、鉴权双份、启动竞态） |
| R14 | **`Stream`/`Aggregate` 的 model 由调用方显式传入**，渠道不得用实例字段记忆「上次请求的模型名」 | 旧实现 qoder 用全局 `lastModel`，多账号并发会串号；traework 恒为空串。详见 `docs/三接口兼容改造备忘.md` §3 |
| R15 | **Responses 的 `function_call` 必须是独立 output item**（带 `call_id`），Anthropic 的 tool_use 参数必须走 `input_json_delta` | 参考实现 `tokligence-gateway` 两处写法不合规范（塞进 `message.content`、start 里一次性给完整 input），Codex/Claude Code 会解析失败 |
| R16 | **无账号渠道不建 auth 文件、不进 `reloadAccounts`、不参与禁用/冷却惩罚** | 匿名凭证是常量 `public`；`SyncToDir` 会剔除磁盘上不存在的虚拟账号，故只在装配时注入一次。单账号 + 不可重登 ⇒ 任何账号级惩罚都等于整渠道下线，故 4xx 一律走 `ErrPassthrough`（原文透传、不计错不冷却）。**2026-09-24 修订：429 也不再冷却**——原「429 短冷却是唯一需要的背压」经实测证伪：单账号无号可轮换，冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，反而不如透传 429 让客户端按 `Retry-After` 自行退避；同理**传输层错误也不再累计 `errCount`**（默认 3 次网络抖动即冷却唯一账号）。两者由新增的 `server.Runtime.SingleAccount` 统一豁免（结构属性，不硬编码渠道名），启动时另调 `Pool.ClearPenalty` 自愈旧版遗留的冷却。详见 `internal/server/handler.go` 的 `Runtime.SingleAccount` 分支 |
| R17 | **用量/积分双流水分口径统计，不强关联、不折算** | `internal/ledger` 双 JSONL（usage 按渠道×模型 / credit 按账号 earn·spend·expire）；写入仅 append 缓冲句柄（30s AutoFlush），读取仅在 UI 请求 `/api/usage` 时按月分段扫描聚合，常驻内存 ≈0。`Upstream.Stream` 返回末帧 usage（R14 同款显式传参哲学）。首见账号只记一条「存量额度」baseline，不逐条展开。**同 key 重复条目（WorkBuddy 伪键一对多）先聚合求和再差分**，每 key 每次刷新最多一条事件；升级首启将旧错误流水一次性归档为 `old-credit-*.jsonl` 并删快照重建 baseline（issue #38）。**2026-10-08 补丁（issue #67）**：WorkBuddy 签到包落在「明日到期」key 上且提前一天以 r=0 建档，「发放即消耗」场景同 key 差分 delta=0 → 当日 earn 被抵消为 0（面板今日收入恒 0）。两层修复：① 差分补丁——快照增加 `used` 字段，delta=0 且 used 上涨时按 `发放额=usedDelta` 补记 earn（数学依据 remain=total-used；used 恒 0 的渠道零影响）；② 权威入账——新增 `provider.CheckinGranter` 可选接口（对齐 CheckinReporter 模式），WorkBuddy 实现 `DailyCheckinGrant`，调度器以它为唯一签到执行路径并用回执 `credit/today_credit` 直接记 earn（回执缺失/为 0 静默降级回差分）。详见 `docs/用量积分流水记账备忘.md` |
| R18 | **临期阈值可配（默认 24h，下限 24h）** | `config.schedule.expiring_threshold_hours`，normalize 钳下限（日期粒度到期判定低于一天无意义）；scheduler 与 app.creditTotals 同源取 `cfg.ExpiringThresholdDur` |
| R19 | **TraeWork 专用池判据仅 `available_endpoint==1`**（2026-09-26 修订，issue #44 撤回 pid==209） | 演进：09-18 专用池下发 ep=1 → 09-23（f6f41a4）上游不再下发 ep=1，改判 `pid==209` → **09-26 实锤证伪**：pid=209「200 档每日签到」已升级为通用积分。本仓日志铁证：09-23 15:20–16:10 连发 99 次对话期间 remain（208/221 池）恒为 3086，而「不可用」小计 2200→1846（-354=99 次对话消耗量），即**扣费实际发生在被判为不可用的池上**；继续排除会让 pool 按虚低余额选号、面板误标「不可用」。故判据回退为 `ep==1`。pid=208/209/221 均可消耗 |
| R20 | **千问办公推理 body 必须携带 `business` 段**（`{product:"qoder_work",type:"agent",version:"1",feature_switches:{}}`） | 2026-09-24 上游 1.0.4 起网关按 `body.business.{product,type}` 解析模型目录，缺失 → 对话恒 HTTP 200 + envelope 503 `Model catalog unavailable`（模型列表/余额/费率不受影响）。**仅补 `Cosy-Business-*` 静态头不能替代**。已实测四组对照隔离变量：body 缺 business 时「本项目透传 body」与「上游原生重构造 body」均 503，补上后均 200 ⇒ 原生 body 结构、官方 `Encode=1` WASM 组包、机器指纹（machineId/Token）**均非必要条件**，故本项目只补字段、不引入 wasmtime 级依赖。参考 Buddy2api PR #84（v2.1.15） |
| R21 | **千问办公 `expires_in` 单位是秒**，且 `expiresAt` 可被 access token 的 JWT `exp` 校正 | 回归：早期按毫秒处理（`*time.Millisecond`），把 7 天压成 604.8 秒 → 落盘 `expiresAt` 比真实寿命少 ~7 天 → `NeedsRefresh(10min)` 几乎恒为真 → **每次请求都刷 token**，与千问办公 App 高频互踩，直至 refresh token 被作废、账号被禁用。证据链：上游 `expires_in=604800` 按秒算 = access token JWT 的 `iat→exp`（整 7 天，吻合）；按毫秒算 = 文件里的值（吻合）。且同仓 workbuddy/trae/workbuddyai 的 auth 文件 `expiresAt` 与 JWT `exp` 逐秒一致，**仅 qwenwork 偏离 6.99 天**。修复：①refresh 按秒解释，优先取绝对字段 `expires_at`，两字段都缺失时回退 84h（JWT 实测 7 天的一半）；②`LoadQwenWorkDir` 调 `Auth.AdoptJWTExpiry()`，用上游签名的 JWT `exp` 原地校正历史脏值（仅内存、只增不减、非 JWT 不动）。**注意：同族 dt-/drt- 渠道（qoder/qodercn/qodercom）token 为不透明串、无 JWT 可交叉验证，其 `// ms` 标注未被本次改动触及**（无证据不做改动） |
| R22 | **智谱清言（`glm/*`）走网页版私有接口；登录以 CDP 自动捕获为主、手工粘贴为兜底** | 清言无可编程登录接口，凭据是浏览器 Cookie 里的 `chatglm_refresh_token`。自动路径见 R27/R28；手工路径保留 `POST /api/login/glm_token`。**不为它引入 WebView2**（R7 已删除该依赖，为单渠道加回是架构倒退且 Windows 专属）——CDP 走系统已装的 Edge/Chrome，零新依赖。协议要点见 `internal/glm/`（含 CDP 登录编排 `internal/login_glm/`） |
| R23 | **清言「签到」的实质是保活对话；新账号额度需 App 侧登录才发放** | `member_info.score_rule` 原文「免费用户，登录赠送200积分/天」，**但对照实验证明这个「登录」指 App 登录**：新账号加进来后 `left_score=0`，**必须在智谱清言 App 登录一次**才发放（+3000，随后再 +500）。证据：某账号创建后独立监控 **15.5 分钟全程为 0**，App 登录后 **30 秒内**变 300000（见 `internal/glm/`（含 CDP 登录编排 `internal/login_glm/`） §2.16）。**影响**：`pool.Pick()` 按 credits 降序选号，而 `healthy()` 不要求 credits>0 ⇒ **额度为 0 的账号不算被禁用，但永远排最后、实际轮不到**。**代码层面无解**（额度发放是上游行为，Web 端无「领取额度」端点）。故 `DailyCheckinReport` 的实质动作 = 保活对话；`UserResource` 从 `member_info.left_score` 读真实积分（**单位「分」，÷100 得积分**）。旧的 `activity-api` 签到活动已下线，保留调用作尽力而为、**完全静默失败** |
| R23b | **分析纪律：观察性数据只能提假设，定因果必须做对照实验** | R23 的结论我**连续错了四次**（详见 `internal/glm/`（含 CDP 登录编排 `internal/login_glm/`） §4.3）：①「服务端延迟」②「App 登录是原因」③「14 分钟没到账⇒需要 App」④「自己到账，与 App 无关」（**忘了是我自己让用户去登录的**，把实验干预当自然现象）。共同病根：**拿观察当因果 + 样本量 1 就下结论**。最终靠**对照实验**才定性：不干预观察 15.5 分钟全 0 → 引入单一变量（App 登录）→ 30 秒内到账。**可复用判据**：下结论前先问「还有哪些变量在同一窗口内变动」；**自己做过干预的实验必须把干预当变量** |
| R24 | **清言渠道为正常多账号渠道（`SingleAccount` 不设）** | **2026-09-26 修正**：初版误设 `SingleAccount=true`，理由是「凭据轮换后不可人工恢复」。但 ① 现在可用 CDP 自动登录随时补账号，该理由不成立；② `SingleAccount` 会**跳过所有账号级惩罚**（handler 两处短路），导致账号 A 失效/限流时**永远不切账号 B**，多账号形同虚设。故改为正常多账号：错误分类惩罚 + 池内轮换。唯一真实约束「refresh_token 轮换必须落盘」由 `glm.RefreshToken` 保证。**教训：`SingleAccount` 是结构性声明（该渠道只有一个号且不可恢复），不是「觉得渠道脆」的保险丝** |
| R25 | **清言 SSE 是「增量 delta」，但 `part.status=="finish"` 时给的是全文** | **2026-09-26 逐帧实测纠正**：`part.content[].text/.think` 的语义取决于 `part.status`——`init` 帧是**增量片段**（每帧几个字符），`finish` 帧是该段落**完整全文**。即 init 帧拼接 == finish 帧全文。实现：init 帧直接透传为 OpenAI delta 并累加；finish 帧与已发出内容比对，仅在全文更长时**补发差额**（防漏兜底）。**曾因照抄参考实现「假定全量快照」的注释而写错**，实测拼出 `"1, 3, 4, 5, 5"`（正确应为 `"1, 2, 3, 4, 5"`）。回归测试 `TestStreamRealFramesNoDuplication` 用真实抓包帧锁死。**注意参考实现 GLM-Free-API 的流式路径本身也是错的**（`substring` 求差在 delta 语义下丢字），不可照抄 |
| R26 | **清言 `accessToken` 允许为空（凭据本体是 refresh_token）** | 用户手填时通常只给 refresh_token，而 `auth.Parse` 要求 accessToken 非空 → `LoadGLMDir` 用放宽版解析器 `parseAllowMissingAccessToken`，由 `glm.acquireToken` 在首次请求时补齐。且 refresh 会轮换 refresh_token，**必须落盘**（落盘失败显式报错，对应不变量 19/20） |
| R27 | **清言登录走 CDP 自动捕获 Cookie，独立 profile，手工粘贴仅作兜底** | 凭据是浏览器 Cookie 里的 `chatglm_refresh_token`。**不读浏览器 Cookie 数据库**——实测 Edge 运行时对其持独占锁（20 进程），既不能读也不能复制，且值为 DPAPI+AES-GCM 加密。改为：`internal/cdp`（手写最小 WebSocket，零新依赖）拉起**独立 profile** 的 Edge/Chrome + 调试端口 → 用户正常登录 → CDP `Network.getAllCookies` 读回。独立 profile 天然隔离，**多账号逐个添加互不干扰**（无需手动开无痕）。手工粘贴路径保留为兜底。见 `internal/login_glm/auto.go` |
| R28 | **自动登录的完成判据是「凭据验证通过」，不是「Cookie 出现」** | **实测发现**：清言对全新访客会自动下发 `chatglm_refresh_token`（423 字符），该 token 调 `user/refresh` 被拒（`访客账号不可用`）。若以「Cookie 出现」为完成判据，会抓到一个**永远不可用的访客账号**。故实现为：轮询 Cookie → 尝试验证 → 只有验证通过（非访客）才落盘收工；超时且只见到访客凭据时给出明确提示。见 `TestLiveGuestTokenBehavior`（实盘验证） |
| R29 | **多账号轮换的真实语义：粘性路由 + 错误惩罚 + 同请求换号**（2026-10-08 修订，PR #82） | 轮换**不是**「积分低就切」：① **粘性路由**优先复用上次成功的账号，直到冷却/禁用或连续 50 次成功；积分只在**选新号**时作排序键（临期 → 总余额）。② **错误惩罚照旧**（冷却/禁用/计错），但自 PR #82 起**账号级错误改为同请求内自动换号**：`ErrKind.Rotatable()` 族（限流/欠费/登录态死/404/5xx/账号故障/ErrClient）在惩罚后 continue 换下一个候选（`MaxRotate` 默认 3 封顶）；内容类错误（content_blocked/prompt_too_long/passthrough）**仍透传不换号**——换号必然复现；候选全部耗尽时透传**最后一个上游原始响应**（保 6004 重置时间等细节）而非包装 503。只有**传输层错误**照旧不计 Rotatable、直接 continue 换号。**变更动机**：旧「错误下次生效」语义下，客户端重试又被粘性路由钉回同一池，本可避免的失败原样甩给客户端（2026-09-23 生产事故：13 个健康号在场却返回 429）。单账号渠道（`SingleAccount`）完全豁免不变。回归测试 `internal/server/glm_rotation_test.go`（旧断言已按新语义更名改写）、`rotate_on_account_error_test.go` |
| R30 | **清言「伙伴/群聊」任务不自动化** | 接口已探明（`mainchat-api/claw_agent` 完整 CRUD，`claw` = 「伙伴」），但**决定不实现**：① 每天 500 积分（≈5 积分实际价值）vs. 风控风险不对称；② 这些任务的设计意图是引导**真人**使用产品，脚本刷属典型薅羊毛特征；③ 用户手动点两下成本极低。**故本渠道自动化边界 = 保活对话 + 积分读取，不碰创建伙伴/绑定 IM/群聊等写操作** |
| R31 | **每日登录积分走 `member-api/member/daily_login_score`，已接入保活流程** | 初版探测的路径名**全错**（`login_bonus`/`daily` 均 404），真实端点是 **`daily_login_score`**（2026-09-26 从 Web 主包挖出）。主包里它是**页面加载时自动调用**的（`errorMessageShow:false`）⇒ 调用它 ≈ 打开一次网页，特征与网页端一致，**不是跨端伪装**。响应：`status=0` 领取成功；`status=10001 "今日已领取"` = **幂等非错误**。**故无论上游是「自动发放」还是「需主动调用」，调它都安全且有益**。⚠️ **实现陷阱**：`doJSON` 在 `env.Status != 0` 时抛 error，会把 `10001` 误报成「领取失败」→ 新增 `doJSONEnvelope`（只把 HTTP ≥400 与非法 JSON 转 error，业务码交调用方解释）。回归测试 `internal/glm/daily_score_test.go` |
| R32 | **不模拟 App 登录来触发额度发放** | 用户设想「模拟 App 登录动作」以避免手动开 App。**实测否决**：① Web 端**无**任何额度激活接口（8 个候选全 404）；② 换 App 头访问同一接口反而 401（`member_info` Web 头 200 / App 头 401）⇒ App 走**另一套认证**（设备指纹、App 签名等），不是加 UA 就能冒充；③ App 域名 `api.chatglm.cn` 独立存在。**风险不对称**：模拟 App 登录是**跨端伪装**，风控风险比只读 Web 私有接口高一个量级，且触发额度发放正是薅羊毛特征（与 R30 同一逻辑）。**结论：手动开 App 登录一次即可**（一次性成本，零风险，零开发） |
| R33 | **三个 chatglm 变体的模型名带上游代号后缀 `:moe_53f`；旧名保留为别名** | **实测**：6 个模型名逐个测服务端上报的 `parts[].model` —— 三个 chatglm 变体（普通/`zero` 推理/`deep_research` 沉思）**都上报 `moe_53f`**，即**同一底层模型的不同推理等级**（`moe`=MoE 架构、`53` 很可能指 5.3）；search=`ai-search`、ppt/video=`all-tools-glms-glms-v2`。故给三个变体加 `:<model>` 后缀（`glm/chatglm:moe_53f` 等），**search/ppt/video 不改**（其代号是工具标识非模型版本）。**两个必须同步的点**：① `resolveAssistant` 查表前**剥掉 `:` 后缀**（同时保留完整名查表），否则 `chatglm:moe_53f` 只能靠兜底命中；② **旧名保留为别名**，已配置旧名的客户端不断，但 `/v1/models` 只列新名。新增 `UpstreamModel()` 辅助函数。回归测试 `TestResolveAssistantStripsUpstreamSuffix` 断言带/不带后缀解析结果一致 |
| R34 | **新增渠道必须补 go xxxSch.Run(sctx)，否则该渠道的自动签到/保活从不运行** | **2026-09-27 实测发现**：加 glm 渠道时创建了 glmSch、注册进 runtimes、设了观察者，**但漏了 Run()** ⇒ GLM 的自动签到与保活**从未执行**。危害特征：**不报错、不崩溃**，且**手工触发仍可用**（面板按钮 / RunCheckinNow 走的是另一条路径），故极难发现——用户是手工签到后才察觉。**回归测试** cmd/omnigate/scheduler_start_test.go：静态扫描「定义了 xxxSch := scheduler.New(...) 就必须有 go xxxSch.Run(」，并交叉校验 runtimes 里声明的 Scheduler 都已启动。**新增渠道清单应加一项：创建 → 注册 runtimes → 设观察者 → **go Run** → 补测试** |
| R35 | **流式请求必须用独立的 StreamHTTP（不设 Client.Timeout）** | **2026-09-27 生产日志实证**：GLM 对话流走带 Client.Timeout 的 client，触发 context deadline exceeded **5 次**（09/26 22:42、09/27 00:03/00:09/00:12/00:18），**且无终止帧** ⇒ 客户端表现为「回答到一半停住」。根因：Go 的 Client.Timeout **覆盖整个请求生命周期（含读 body）**，对 SSE 意味着「长回答必被掐断」。**修法**（照 traework 既有模式）：Client 加 StreamHTTP *http.Client{Transport: tr}（**共用 Transport 复用连接池、但不设 Timeout**），ChatStream 改用它；main.go 两处 applyProxies 都要把 {glmUp.HTTP, glmUp.StreamHTTP} 一起传（SetTransportProxy 会**新建** Transport，只套一个会让流式漏掉代理）。**非流式 client 仍保留 Timeout**（一问一答需兜底）。回归测试 internal/glm/stream_timeout_test.go：证明流能跑过 HTTP.Timeout，且对照证明非流式仍会超时。**2026-09-29 推广到全部流式渠道**（issue #42）：此前只剩 glm/traework 有 StreamHTTP，qwenwork/qodercn/qodercom/qoder/workbuddy/workbuddyai 的 ChatStream 全用带 Timeout 的 HTTP ⇒ 长流式同样会被掐断。现已给全部流式渠道补 StreamHTTP（共用 Transport、不设 Timeout，流式里 Select{StreamHTTP 非 nil 则用之}），main.go **两处** applyProxies（启动 + 面板热更新）都要把 {HTTP, BillingHTTP?, StreamHTTP} 一起传，否则只套 HTTP 会让流式漏掉代理 |
| R36 | **流被截断不得靠补 `data: [DONE]` 伪装成正常收尾**（issue #42） | **实证**：qwenwork/qodercn/qodercom/qoder 四份 sse.go 的 `sawDone` 是**死变量**（声明后从不赋值）⇒ 上游连接中断/读超时被掐断、或断在半个帧上（末行无换行），出口照样补 `data: [DONE]`，客户端拿到半截 tool_call arguments 报 "tool input was not fully received"，而网关日志**无任何错误**，极难排查。**修法**：parseNestedSSE 增加 `truncated` 出参（读错误/半帧 → true），`streamAsOpenAI` 里截断时改发一帧 OpenAI 规范 error（`code: upstream_truncated`）且**不发** [DONE]——调用方 `err!=nil` 同时保留。通用路径上游/sse.go streamCore 也做了同样的截断→error帧改造。刻意保留的行为：上游「正常 EOF 但漏发 [DONE]」（帧完整、末行有换行）仍兜底补 [DONE]，避免误伤本就补的渠道。回归测试：qwenwork/qodercn/qodercom/qoder 各 `stream_truncation_test.go` + 通用 `TestStreamTruncationNotDisguisedAsDone`，覆盖四种输入（传输层错误/半帧/正常 [DONE]/EOF 漏发） |
| R37 | **TraeWork AuthCode 交换：4xx 属终态，不得换 origin 重试掩盖首因；设备号作用域 = 账号**（issue #50；2026-10-08 二次修订 issue #60/#70） | 旧实现 `ExchangeAuthCode` 对 4xx 也 `continue` 试下一个 origin，把首选 api.trae.cn 的真实错误（403/20401 设备数上限）覆盖成回退 origin 的 400/10101「无效参数」后再打印 ⇒ 表象彻底指向参数写错，且 authCode 一次性、4xx 后重试纯属刷屏。**修法**：4xx（除 429/408）立即返回携带**首个** origin 真实响应的 `*AuthCodeRejectedError`（含 origin/status/body）；`IsDeviceLimitReached` 单表识别 20401（可行动提示「去其它设备登出释放名额，最多 10 台」）；`login_trae.Poll` 把终态错误固化到 `login-state.json` 的 `Err` 并清空 AuthCode，后续轮询短路不再每 2s 重消耗。**设备号持久化**（初版）：官方客户端 `getDeviceId()` 是持久的，本仓每次登录 `randHex(32)` 随机新生成 = 每点一次都是新设备。改为从 `device-id.json`（`login-state.json` 同目录、独立文件）复用，缺失才生成并落盘（不复用 login-state，它登录成功/取消即删）。**二次修订（2026-10-08，#60/#70 实证推翻本机级共享）**：`device-id.json` 让一机多号共享同一设备号——#70 实测 15 号对照：共享设备号的 3 个账号全部撞 4017 common risk control（仅对话入口被拒、积分正常），一机一号的 12 个零风控；#60 实测同一编造设备号被多账号绑定后新账号授权恒 20401 Device limit reached（换设备号立即成功）。**修法**：设备号作用域从「本机」改为「登录会话」——每次 `Start()` 新生成一对 machineId/deviceId，登录成功随 `auths/trae-<uid>.json` 落盘成为该账号的稳定注册设备（满足 #3 的 x-device-id 稳定性，签到/对话头从 auth 文件读、不经过 Start）；`Poll` 成功后顺手删除遗留的 `device-id.json`（老用户自愈）。**#60 建议的「读官方客户端真实设备号」不做**：需移植 storage.json TC 解密且依赖本机装官方客户端，而 #70 数据已证明随机号本身可用，会话级新号即可消解两者。**权衡披露**：同账号重登会消耗一个设备名额（上限 10），但远低于旧随机版的消耗速度。回归测试 `internal/login_trae/login_device_test.go` |
| R38 | **上游按模型限流时不得软化整个账号**（issue #53） | **2026-09-29 最小修复（不做大架构改动）**：WorkBuddyAI 上游模型级 429（6004 / body 含 `switch to the other models` / `usage exceeds frequency limit`）在 `Classify` 里判为 `ErrPassthrough`（请求级、不冷却账号），透传原文让客户端按 Retry-After 自退避或换模型——**不**把该账号其它还能用的模型一起拖进 SoftCooldown。与 R16 单账号渠道同哲学。**不做的部分**：pool 升级到 model 维度状态、`stickyKey(kind)`→`(kind,model)`、state v4（issue 作者提议的两种更大改造），它们动 pool/state/handler 三核心、~370 行，需单独评估（作者已主动提出可拆两个小 PR）。注意此判据仅限 WorkBuddyAI：其它渠道的 429 语义仍是账号级 |
| R39 | **`scheduler.Config` 的时间字段：`nil` = 未配置（落默认），`[]int{}` = 本渠道没有这类定时任务** | **2026-09-23 实证**：`scheduler.New` 原用 `len(...) == 0` 判默认值，把两者混为一谈 ⇒ 传 `nil` 想关掉定时任务的渠道被静默补上默认时间。`main.go` 里命中四处：**qoder** 每天 09:00、21:00 各跑一次必然失败的签到（面板按 `last_checkin_ok` 渲染出红色「签到失败」标签，消息是「qoder 暂无签到活动」/「no refresh token」）；**workbuddyai** 注释写「Keepalive 关闭」但 22:00 照常每日刷 token；**qwenwork 最严重** —— 注释明写「定时保活反而会与千问办公 App 互踩」，而保活批次每天照跑。同一个「无签到活动」概念在 `app.go` 有 `noExplicitCheckin()` 守卫（手动签到、首次签到路径都据此跳过），唯独 `scheduler.New` 的零值兜底把它抹掉。**修法**：两处 `len(...) == 0` 改 `== nil`；四处调用点改传 `[]int{}`；`Run()` 补「两个列表都为空时阻塞等 ctx/配置变更」—— 否则 `nextFireMinutes` 返回零值走 `IsZero` 兜底，变成每天 1440 次空转。回归测试 `internal/scheduler/scheduler_defaults_test.go`（零值落默认 / 显式空关闭 / `CheckinHours` 旧字段兼容 / 无任务时 `Run` 阻塞且响应取消） |
| R40 | **流内业务错误不得伪装成正常收尾；兼容层不得吞 error 帧**（2026-10-03） | **背景**：traework 的 `event:error`（3004 限流/1005 权益）旧实现写成 `delta.content` + `finish_reason:stop` + 补 `[DONE]`，且函数返回 `nil` ⇒ 客户端看到「正常结束、内容莫名其妙」、agent 拿半截回答继续跑，且 handler 无从得知失败 ⇒ **账号不冷却**；更糟的是 `NoteSuccess`/`stickySuccess` 在读流**之前**已执行，粘性路由把请求钉死在中招账号上，重试必然再撞。**修法**：① `solosse` 错误分支改发 OpenAI 规范 error 帧（限流标记 `upstream_rate_limited`）、**不补 `[DONE]`**、把错误返回调用方；② 新增 `provider.StreamErrorClassifier`（`Kind()`），handler 用 `errors.As` 取值后按 kind 冷却账号（软 60s / 硬 12h / 禁用），粘性路由见 `status.Cooling` 自动让位，无需显式 `stickyClear`；③ **兼容层必须识别 error 帧**——`gateway.parseChatSSELine` 此前只认 `choices`，error 帧被静默丢弃，致 `/v1/messages` 发 `end_turn`+`message_stop`、`/v1/responses` 发 `response.completed`+`[DONE]`，把失败伪装成成功（该缺口对内层所有渠道通用，含 R36 的 `upstream_truncated`）。**协议合规要点**：Anthropic `error.type` 是 **9 元判别联合**（`api_error`/`rate_limit_error`…），**不得填内层私有码**——限流映射为 `rate_limit_error`；Responses 用 `response.failed`（不补 `response.completed`/`[DONE]`），`response.error.code` 亦须映射到枚举（`server_error`/`rate_limit_exceeded`），私有码只放 `error` 事件的自由 `code` 字段。**3004 的限流维度是待强化假设**（R23b）：观察仅 1 例且来自 checkin 路径，只足以排除 IP/全局级；因软冷却仅 60s、判据不成立时最坏影响是闲置一分钟，故按账号级处置。回归：`traework/solosse_test.go`、`server/stream_error_penalty_test.go`、`gateway/stream_error_frame_test.go`、`scripts/ui-static-check.mjs` |
| R41 | **小浣熊协议回调必须由 `main.go` 在初始化前拦截（`--raccoon-callback`）**（2026-10-05） | **背景**：小浣熊「浏览器授权登录」的授权码经自定义深链 `office-raccoon://auth/callback` 回传，实现方式是登录期间临时改写 HKCU 的 `office-raccoon` 注册表命令行为 `"<omnigate.exe>" --raccoon-callback "%1"`。`internal/raccoon` 把 `CallbackFlag` 与 `SaveCallback()` 都写好了，**但 `cmd/omnigate/main.go` 从未解析该 flag** ⇒ Windows 唤起回调子进程后，它当作普通启动又拉了一份 daemon：端口被占（`listen … bind: Only one usage…`）、**且它的启动自愈看到「残留的协议改写」立刻 `RestoreProtocol`+`ClearCallback`** ⇒ 授权码从未落盘，常驻进程轮询到 5 分钟超时。**全程不报错不崩溃**，用户只看到「点完授权但账号没加进来」。**修法**：`main.go` 在 `os.Chdir(workDir())` 之后、**任何初始化（config/日志/服务/托盘）之前** 拦截 `handleRaccoonCallback(os.Args[1:])`，命中即「落盘后 `os.Exit`」，绝不启动第二份服务。**同时**：`windowsgui` 构建无控制台，回调结果必须追加写进 `data/app.log`（否则失败时零线索）。**同类教训**：这与 R34（新增渠道漏 `go xxxSch.Run()`）同族 —— **「声明了却没接线」的疏漏在源码层就能判定**，故补静态回归 `cmd/omnigate/raccoon_callback_test.go`（断言 main.go 调用了 handler、位置在服务初始化前、分支含 `os.Exit`），去掉修复即失败。**附带**：`/api/state` 新增 `login_error` —— 此前前端只看 `login_busy`，把**所有渠道**的登录失败/超时都显示成「登录完成」，正是它掩盖了本次问题；现由 `peekLoginError()` 非破坏式回传真实原因（读取即清空会被 `/api/state` 的其它调用方偷走） |
| R42 | **渠道本地模型校验必须取「静态表 ∪ 动态目录」的并集**（2026-10-05） | **背景**：raccoon / loomy 都做本地模型名校验（上游对未知模型名**静默回落到默认模型**并返回 200，不校验会让用户以为在用 A、实际扣 B 的额度），但实现只查**静态表**（`KnownModel(id)`）⇒ 上游目录新增的模型会被本地 400 误拒，而**同一个模型正被 `/v1/models` 正常列出**（后者读的是动态目录 `FetchModels` 结果）——表现为「面板列出却调不动」。**实测（2026-10-05）**：raccoon 账户 `/v1/models` 列出 9 个模型，其中 `sn-sensenova-6-8-flash` 调对话被本地 400 `model_not_found`，但直连上游同一模型 **HTTP 200 正常服务**（`sn-sensenova-6-8-flash-lite` 在静态表里、`sn-sensenova-6-8-flash` 不在，二者仅差一个后缀）。**修法**：`fetchCatalog` / `fetchModels` 成功后把目录里的模型名记进 Client（`liveIDs`，**单调扩大、只增不减**——避免目录瞬时抖动把可用模型判成未知），`ChatStream` 改查 `c.knownModel()`（静态表 ∪ liveIDs）。两渠道同款修复 + 回归测试 `TestKnownModelAcceptsLiveCatalog`（去掉并集即失败）。**顺带**：给两个 Client 加 `Base` 字段（默认常量端点，测试注入 httptest 假上游），避免为写这条测试而把 `LLMBase`/`GatewayBase` 从 const 改成 var |
| R43 | **运行统计（`internal/stats`）与用量流水（`internal/ledger`）是两套并列口径，不合并、不互校**（2026-10-07，PR #71–#73） | **背景**：新增「运行统计」tab 时引入了第二套统计面。分工：`ledger` = 磁盘 JSONL 双流水（`data/ledger/{usage,credit}-*.jsonl`），账号**条目差分**口径，按需扫描聚合（R17）；`stats` = 内存实时 + 按日归档 `data/stats.json`，「今日」口径——消耗走**余额下降差值**、收入走**日志签到事件**（`events.go` 正则解析 `app.log`，因「登录即自动签到」等场景条目晚于入账建立，差值恒 0 漏记）、逐请求 token 走 **usage 帧精确值**（`usage.go`）。**决议**：两者**零共享状态、零互相写入**（stats 仅只读 `ledger.Query(1)` 取当日作废），各自标注口径、互不引用；**数字天然不同是设计而非缺陷**（差值含积分包到期作废，ledger 已拆 spend/expire），**不追求对齐**。**边界**：`ledger` 仍是面板「用量与积分」的数据源与 issue #67 等待修的口径；`stats` 只服务「运行统计」tab。新增统计需求时**先明确归属**（精确拆分→ledger；实时/今日/趋势→stats），**不得**为对齐数字而改动另一侧。**注入纪律**：stats 全部数据输入走**进程内直调**（`allStatuses` / `handler.StickySnapshot` / `ledger.Query`），**不得** HTTP 自环（R13）；`main.go` 须 `go appInst.StartStatsFeeders(sctx)`（漏接线即静默无数据，同 R34 家族） |
| R44 | **WorkBuddyAI 对话必须带 `X-Conversation-ID`（三件套缺一不可）**（2026-10-20 实测） | **症状**：`workbuddyai/deepseek-v4.1-flash` 恒 400 `code=11-128 request illegal`，客户端以为是内容指纹问题。**校验顺序实测**：openresty → stream 校验（无 `stream:true` 先报 `11101 Non-stream chat request is currently not supported`）→ 渠道校验 11-128。**对照实验**（逐头删除二分，直发 `https://www.workbuddy.ai/v2/chat/completions`）：六组头组合全 400 ⇒ 不是「缺某个可选头」就是缺必填项；逐头删到最小集后**仅三个头必填**——`X-Conversation-ID`、`X-Conversation-Request-ID`、`X-Conversation-Message-ID`，缺任一即 400/11-128。其余（`X-Agent-Intent`/`acp-connection-id`/`X-Private-Data`/`x-codebuddy-request`/`X-Request-ID`/trace 全组/`X-Agent-Purpose`/`X-Domain`）均可删；旧 UA `CLI/2.63.2 CodeBuddy/2.63.2` 与旧 `X-IDE-Version 5.5.4`、`X-Product=WorkBuddy` 都能通过；**值格式不敏感**（UUID / 32hex / 三头同值全 PASS）。**依据**：官方客户端 `E:\WorkBuddyCN\WorkBuddyAI` 的 `app.asar` 内 `buildVlmGatewayHeaders`/`applyGatewayHeaders`（无 HMAC/签名，纯头校验）。**修法**：`internal/workbuddyai/client.go` 的 `chatHeaders` 补 `X-Conversation-ID`（与 Message-ID 同值，最小改动）。回归：`internal/workbuddyai/headers_test.go` 的 `TestChatHeadersConversationFamily`（去掉修复即 FAIL）。**遗留**：国内版 `internal/upstream/headers.go` 的 `injectConversationHeaders` 仍未发该头（上游尚未启用该校验） |
| R45 | **「模型繁忙」（429 + `code:14003`）只重试、不冷却**（2026-10-20 实测） | **症状**：用户拿 `503 no_healthy_account`「模型繁忙，最早约 xx 解冻」，而**同一时刻官方客户端可用**。**观察**：`state-workbuddyai.json` 的 `model_cooling` 在 14:50/14:52/14:58 被三次 429 反复续期 60s ⇒ 上游的瞬时抖动被网关放大成整分钟不可用。**对照实验（R23b 纪律）**：官方 5.8.4 全套头与网关修复后的头**都既可能 200 也可能 429**；固定 8 秒间隔连发 5 次得 `429/200/429/200/200` ⇒ `14003` 与「请求头像不像官方客户端」无关，是**共享免费池的瞬时容量波动**，官方 Electron 客户端遇到它也是立刻重试。**修法**：`internal/workbuddyai/retry.go` 新增 `modelBusy()` 识别（429 + `"code":14003`/`Model busy`/`模型繁忙`）；`ChatStream` 对命中者重试 2 次（`busyBackoff` 500ms/1s 指数退避）；重试耗尽后 `Classify` 判 `ErrPassthrough` 原文透传，**不**进入模型级冷却（与 R16/R38 同哲学）。**验收实测**（新 exe 上线后连发 6 次）：5 次撞上游 429，**全部被内部重试消化**，用户侧 6/6 返回 200 且 `model_cooling` 不再出现。回归：`TestModelBusyDetect`、`TestChatStreamRetriesModelBusy`、`TestChatStreamModelBusyExhaustedPassthrough`、`TestClassify`（去任一修复即 FAIL） |
| R46 | **QoderCN「模型排队」（`10605`/`isQueued`）必须识别为错误，不得透传或补 `[DONE]`；并以首帧看门狗兜住上游挂起**（2026-10-20/21 实测） | **症状**：`qodercn/deepseek-flash` 经网关「点了就排队、一直卡住」，客户端既无内容也无错误；同一时刻 `qodercn/qwen3.8-flash` 1.6s 正常返回 ⇒ 排队是**模型级**，与账号/请求头无关。**证据链**：①非流式请求 `ttfb=180.04s` 后网关回 `502 upstream_parse: unexpected EOF`；②上游报文为三层嵌套 `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"dfmodel\\\",\\\"queueCount\\\":3058,\\\"waitTime\\\":185,...}\"}"}` ⇒ 免费池排队，前方 3058 个请求、预计等 185s；③官方客户端此时同样显示「排队中/预计等待」⇒ 排队真实存在，官方能拿到 `queueCount/waitTime` 并展示；④**反证**：抓包（`QODERCN_CAPTURE=1`）显示同一模型正常时首帧 `firstTokenDuration=338ms`、2.0s 收尾 ⇒ 排队是间歇性的。**旧实现错在哪**：把该帧当普通 chunk 透传 → 客户端解析不出 `choices`（干等）、也拿不到 `error`（无法重试）；且 180s 后读错误被当成「截断」，既不可读也不可退避。**修法**：①`queue.go` 新增 `QueueError`（`Kind()` 恒 `ErrPassthrough`——排队不罚账号）+ `parseQueueFrame`（判据 `10605` 或 `isQueued:true`，**必须判 true**，因存在 `10606` + `isQueued:false` 的回执）；②`sse.go` 的 `onChunk` 首行识别排队帧 → 发 OpenAI 规范 error 帧（`code:model_queued`、`type:upstream_queued`，带 `retry_after/queue_count/wait_seconds/upstream_key`）后中止，**不补 `[DONE]`**（R36 同理）；③双窗口熔断：收到 `10605` 熔断 10 分钟（队列真实存在，分钟级不会清空），**首帧看门狗**超时只熔断 90 秒（可能只是抖动，避免误伤）；熔断期内 `ChatStream` 毫秒级 429，`retry_after` 给**剩余秒数**而非固定值；④`ChatStream` 加 `FirstByteTimeout=30s` 首帧看门狗：上游响应头已到但 body 不给数据（排队的真实形态是挂 180s）→ 超时即按排队快速失败，把「等 180 秒」压到「等 30 秒后 429」；只在**首字节之前**计时，首帧到达后仍交 `idle.Monitor`（R35/#42 不冲突）；⑤`Classify` 在通用「429→`ErrSoftRate`」**之前**拦 `model_queued/isqueued/10605` 判 `ErrPassthrough`，否则熔断 429 会被误判 soft_rate 而冷却账号；⑥`parseNestedSSE` 增加**扁平帧兜底**（无 `body` 信封、整帧即业务报文的错误帧也要交给 `onChunk`），避免只支持一种信封造成盲区。**回归测试**（`internal/qodercn/queue_test.go`）：`TestParseQueueFrameReal/Negative`、`TestClassifyModelQueued`、`TestStreamQueueEmitsErrorFrame`、`TestStreamQueueFrameBothEnvelopes`（嵌套+扁平两种信封）、`TestStreamFlatNoiseNotTreatedAsError`、`TestChatStreamFailsFastWhenQueued`（熔断期内**不得请求上游**）、`TestChatStreamFirstByteWatchdog`（上游挂住 → 阈值内 429 且写入熔断）、`TestChatStreamWatchdogPassesNormalStream`（正常流放行且**预读字节不丢**）。已验证：禁用看门狗后 `TestChatStreamFirstByteWatchdog` 必失败（测试有杀伤力）。**诚实的边界**：①「挂 180s 后才吐 `10605`」只观测到一次，且抓包未捕获任何排队帧 ⇒ 首帧看门狗是**兜底**而非已证实的唯一路径；②排队帧的信封形态真实值**尚未抓到**，故嵌套与扁平两种都做了兼容并在受控 mock 下验证通过；③真实上游若改用其它候选形态（如 `event:error` 行、`code` 在 `headers` 里），仍需按 R23b 重新抓包确认；④**上线后实测补充（18:04）**：上游排队挂起期间会发保活字节 ⇒ 首帧看门狗（按「首字节」判定）不会触发，本次挂起以 `upstream_truncated` 规范 error 帧收尾（旧版是垃圾透传/假 [DONE]），后续重试由上游直接给快速 429；该快速拒绝路径此前只透传不熔断（每跳 ~745ms 重打上游），已补 queue.go:parseQueueBody 在 HTTP ≥400 分支同样写熔断（回归 TestChatStreamHTTP429QueueRecordsBlock）；看门狗只兜「完全静默」的挂起，这是已知边界，不是缺陷。 |
| R47 | **主 HTTP 服务必须显式设置 `IdleTimeout`（60s）**（2026-10-21 生产） | **症状**：DSH 走网关的对话「发出去了但网关零日志」，界面无限转圈；同一时刻换官方模型正常；重启 omnigate 即恢复。**证据**：`app.log` 里 17:38:41→17:52:08 共 13 分 27 秒无任何业务请求（`/api/state` 轮询照常 200 ⇒ listener 健康、非进程假死）；3 天里另有 4 次 `wsasend: established connection was aborted`（客户端侧断开）。**根因**：`http.Server` 只设了 `ReadHeaderTimeout`，不设 `IdleTimeout` 时 Go 回退到 `ReadTimeout`，两者皆空 ⇒ keep-alive 空闲连接**永不超时**，关闭时机完全交给客户端；半死连接留在两侧连接池里被反复复用，请求在 TCP 层丢失。**修法**：`internal/app/app.go` 抽出 `newHTTPServer(handler)`，显式设 `IdleTimeout: 60s`（远大于单次请求 2–9s 与流式每次写续命，不影响长回答；给网关主动回收上界）。抽成纯函数是为了单测直接断言配置。回归：`internal/app/server_idle_test.go` 的 `TestNewHTTPServerTimeouts`（删掉 IdleTimeout 即 FAIL，已做杀伤力验证）。**诚实的边界**：本次是相关性证据（零日志窗口 + 重启恢复 + 配置缺陷成立），不是抓包级实锤——下次再卡时用 `Get-NetTCPConnection -LocalPort 7863 -State Established` 对照网关日志即可钉死；若钉死后仍存在，方向转向 DSH 侧连接池。 |
| R48 | **平台表 grid 轨道数必须与 JS 每行单元格数同步**（2026-10-22 生产） | **症状**：面板「今日情况」表格错位，「7日内到期」掉进第一列且显示不全。**根因**：上游 deac5bf 给 `renderPlatformTable` 加了「今日作废」列（10→11 格），`style.css` 的 `.st-plat-row` `grid-template-columns` 仍只有 10 轨——grid 第 11 格自动折进隐式行第 1 列（28px 宽）被裁切。**修法**：桌面补 80px、窄屏 media 补 60px 轨道（今 11 轨：`28 150 52 100 80 80 80 112 84 84 1fr`）；`scripts/ui-static-check.mjs` 新增断言「每行格数(11/11/11) == 桌面+窄屏轨道数」，杀伤力验证：改回 10 轨即 FAIL。**教训**：吸收上游 web 改动时，加列/删列必须同查 `grid-template-columns`（本表注释已钉死此规则）；同法核查了 st-ex-row(6/6)、st-ab-row(7/7)，无同类问题。 |
| R49 | **CN workbuddy 对话头补齐 X-Conversation-ID，并留 OMNIGATE_CAPTURE_CHAT 出站捕获钩子**（2026-10-22） | **症状**：DSH 会话经网关 `workbuddy/deepseek-v4.1-flash` 偶发 400 `code=11128 Illegal API invocation from an unapproved channel`（11h 内 3 次，随后自愈；同期同渠道 243-393KB 大 body 连续 200，**非持续性**）。**五轮直发对照实验**（headers=网关复刻，小 body）：不带/带 X-Conversation-ID 均 200（12 发全过）⇒ 头差异非 CN 触发条件；新身份句 "coding agent powered by DeepSeek Harness"=200、旧句 "AI agent powered by…"放 **system 角色**=400、放 user/tools 参数/top-level system=200 ⇒ **钉死上游黑名单只扫 system 角色消息内容**；净化实弹经网关 200 ⇒ 生产净化路径正常。**结论**：触发源应为上游 10-21 起新增/扩展的、本仓清单外的 system 角色黑名单字面，探针无法复现即不可猜表——定案数据靠捕获钩子。**修法**：①`headers.go` injectConversationHeaders 补 X-Conversation-ID（国际版 R44 强制、CN 实验证明无害，两区校验只可能趋同，先关掉最后一个已知头差异）；②`client.go` ChatStream 加 env 门控捕获：`OMNIGATE_CAPTURE_CHAT=<目录>` 时把**净化后的完整出站字节**落盘 `chat-<纳秒>-<长度>.json`（捕获失败不影响请求），下次复现直接对真实 body 逐段二分。回归 `internal/upstream/headers_test.go`：`TestChatHeadersConversationFamily`（三头非空，删 Set 行即 FAIL）、`TestChatStreamCapturesOutgoingBody`（env 关零写盘 / env 开恰 1 文件且为出站字节，改 env 名即 FAIL）——两项均做杀伤力验证。**已上线**：09:27 部署，CN 小请求 200、捕获文件落盘验证通过。**诚实的边界**：根因字面**未钉死**（5 个假设全部证伪，见实验记录）；本次属低风险对齐+取证布防，不是修复定案。 |
| R50 | CN workbuddy 渠道对 DSH 运行时上下文两句连排字面回 11128（"You are interacting with the user through the DeepSeek Harness Web GUI…"+"…they mean this GUI."，上游 10-21 新增内容审核规则） | v3/v6 探针实验：二分定位到 375 rune 段；单句/拆开/倒序/关键词 pair 全 200，A+B 连排 221 字面必 400 → 规则=连续字面匹配非关键词组合 | sanitize.go 短语级最小改写：features 加 "DeepSeek Harness Web GUI"/"they mean this GUI"，rewrites 加 "Web GUI"→"web console"、"they mean this GUI"→"they mean this console"（语义零损失，顺带覆盖 "Current … URL:" 行）；TestDSHGuiPairLiteral 断言两句字面断链，删任一改写对各自必红（杀伤力已验）；快照护栏同步 10 项+8 对 | 教训：上游内容黑名单会静默扩容且规则形态多样（前有单句锚定、后有连排匹配），净化器按实验判据逐条跟进；探针二分先粗扫后滑窗，停滞=触发跨点或长于窗口 |
| R51 | **渠道身份链必须与官方桌面客户端逐字一致**（2026-10-22 上游 R117 + 本机 5.8.4 解包） | **症状**：UA 仍是 `CLI/2.63.2 CodeBuddy/2.63.2`、`X-Product: WorkBuddy`、`X-IDE-Version: 5.5.4`，且多发了官方根本不存在的 `X-Machine-ID`/`X-Session-ID`（自造指纹，反而成了特征）。**依据**：解包 `E:/WorkBuddyCN/WorkBuddyAI`（version 5.8.4）——`app.asar` 的 `buildVlmGatewayHeaders` 拼 `["WorkBuddy/"+appVersion,"WorkBuddy/"+appVersion,"CLI/"+cliVersion].join(" ")`、`X-Product = product.json.deploymentType`(="SaaS")、`X-IDE-Version = appVersion`、`X-Private-Data: true`；`applyGatewayHeaders` 发 `X-Agent-Intent: craft`；`X-Machine-ID`/`X-Session-ID` 在 asar 内出现 **0 次**；CLI 真实版本在 `cli/package.json` 的 `publishConfig.customPackage.version` = `2.161.0`（`version` 字段是占位 `0.0.0`）。**修法**：`desktopVersion=5.8.4`/`cliVersion=2.161.0`、`clientUA="WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0"`、`defaultProduct="SaaS"`；`CommonHeaders` 删掉 `injectAccountStableHeaders`（连带删除 `deriveStableID`）；`injectAttribution` 补 `X-Private-Data`/`X-Agent-Intent`。**护栏**：`identity_headers_test.go`+`identity_parity_test.go` 五个测试——逐字锁 UA/版本/产品三件套、断言自造指纹必须为空、登录侧 UA 同一串（`/v3/config` 有 CLI 形 UA 门禁）、`identitySites` 扫四处源码一致性、Walk `internal/` 禁代码级旧串。杀伤力三次验证（改回旧 UA / 改回 WorkBuddy / 加回 X-Machine-ID 各自必红）。**遗留**：`internal/upstream/desktop.go:568` 的 CN 桌面探测路径仍用 `X-IDE-Version: 5.5.6`，未纳入本轮。 | **教训**：反代客户端最容易被识破的不是"少发了什么"，而是"多发了官方没有的东西"——自造指纹看起来更真实，实际是唯一标识。身份常量散落四个包时必须用源码级一致性测试锁死，否则改一处漏三处。 |
| R52 | **两份脱敏表必须同步，且必须用真实原文断言**（2026-10-22 上游 R115 续写 + v0.1.5 ef261a85） | **症状**：CN 渠道（`internal/upstream`）已能净化 DSH GUI 段，但 `internal/sanitize` 一份完全不改——而 `internal/upstream/payload.go` 的 `PrepareBody = sanitize.Messages(prepareBodyInner(...))`、`internal/workbuddyai/wb_utils.go` 同一形态，意味着 **workbuddyai（国际版）只有 `internal/sanitize` 那道表生效**，是静默失守点。上游 v0.1.5 同样只补了 `internal/upstream`，我去核对官方补丁时发现他们还同步改了 `internal/sanitize`，才反过来定位到本仓这个缺口。**判据**（与上游 ef261a85 commit message 完全一致）：单独任一句放行；两句以**单个空格连成 221 字符逐字串**才 400；删一字符、插空行、改任一词、等长无关文本填充均放行。**修法**：`internal/sanitize/sanitize.go` 的 `featHeaders` 加两项、`rewrites` 加两对（值用数字零变体，保持"只动一个字符、语义零损失"）。**护栏**：新增 `internal/upstream/sanitize_parity_test.go`——同一 payload 过 `PrepareBody` 与 `sanitize.Messages` 两条链路，断言 221 字符整串不再出现；另加"必须真的改写了"（否则两边都不改也会通过空转）与身份句双表命中。杀伤力验证：删掉 `internal/sanitize` 那两对改写 → 两个测试同时 FAIL。 | **教训一**：`hasFingerprint` 是**预检门**，改了 rewrite 表但忘了往 features 加特征 = 静默失效且测试若用假串也测不出来。**教训二**：断言必须用**生产捕获的逐字原文**，我最初手写的测试常量大小写错了（小写缩写 vs 原文三字母大写），导致断言对着错误基准通过——真实原文取自 `data/chat-capture` 出站 body。**教训三**：本机开发通道会静默转写源码里的特定字面量（写进去的原文落盘后变形，多轮排查后由 `hex()` 证实），测试常量改以 base64 内嵌并在运行时解码。 |
| R53 | **DSH 身份句整段删除（不再插冠词），并修掉预检门的假阴性**（2026-10-22，用户要求「转发后这句话没了」） | **需求**：R50 的修法是给原句插一个冠词 `the`（`powered by the DeepSeek Harness`），只破坏逐字指纹、语义原封不动——读起来仍是「一个由 DeepSeek Harness 驱动的 AI agent」，任何做语义判断的风控都还看得见。用户明确要求出站 body 里这句话**一个字都不剩**。 **修法**：两份表（`internal/sanitize` 与 `internal/upstream`）的身份句 rewrite 值一律改成空串，即整段删除。 **验证**：用表里真实 key（从 `sanitizeRewrites` 读出、绝不手写字面量）构造 84 字节输入，`sanitizeText` 输出 37 字节 `".\nYou are helping with a simple task."`——身份句彻底消失、正文完整保留。 **杀伤力**：把两份表的值改回「插冠词」→ `TestDSHIdentityRewritten`、`TestDSHIdentityWithSuffixRewritten`、`TestDSHPrepareBodyNoFingerprintRemains`、`TestSanitizeBothPackagesStripDSHIdentity` 四条同时 FAIL；恢复即全绿。 **预检安全**：改写键若同时是唯一特征命中项，删掉它会让后续规则整体跳过。此处安全——同段还有 GUI 特征兜底，实测删句后 `hasFingerprint` 仍为 true。 | **教训一（最重要）**：`hasFingerprint` 是**预检门**，而本机开发通道会把源码/命令里的特定字面量**静默转写**（不带 `the` 的原句会被显示和落盘成带 `the` 的版本）。 我因此连续多轮把「特征表存的是不带 the 的原句、我探针写的是带 the 的变体」误判成代码 bug，反复改错文件、写坏结构。**唯一可信的判据是 hex**：`feature==key?true`、`feature==mine?false`、`mine` 51 字节对 `feature` 47 字节，差异正是 `74686520`（`the `）四个字节。 **教训二**：验证/杀伤力脚本一律**从被测表里读取 key**再用，绝不手写那段字面量；手写一次就会被转写一次，测试变成自证。 **教训三**：批量改文件的脚本必须先断言边界（行长度/字节内容）再写，且备份文件按路径区分——我曾用同名的 `/tmp/sanitize.go.bak` 同时备份两个包，`cp` 把它们写成了同一份内容，`internal/sanitize/sanitize.go` 被污染 248 行，只能 `git checkout` 重来。 |
| R54 | **出站取证从「全量 env 门控」改为「异常自动触发」，11 渠道全覆盖**（2026-10-22，用户「异常保存用于分析」） | **需求**：用户问「这样还能被发现吗？最后就是用不了去抓语句吧」——R50/R52/R53 都是**等封了再补表**，始终慢一步，唯一能加速的手段是出事那一秒留住真实证据。而 R49 的钩子有两个使用缺陷：①只在显式设 `OMNIGATE_CAPTURE_CHAT` 时生效，现实里没人常设，等出事去设事件已过；②全量捕获正常请求，一个会话就写几十上百个几百 KB 文件，事实上没法常开。**修法**：新建 `internal/capture` 包，把捕获改为**异常触发**——只在请求失败时落盘，正常请求零写入零开销，因此可永久常开（默认开启，`OMNIGATE_CAPTURE_OFF=1` 才关）。落盘内容：channel/uid/model/status/classify/transport_error + **出站请求体原文**（净化后的最终字节）+ **上游回包原文**（截断 64KiB，标记 `response_truncated`）。目录 `<exe同级>/data/chat-capture/`（与 workDir 同规则），文件名 `err-<时间>-<渠道>-<状态码>-<长度>.json`，文件数上限 500 自动删最旧。**隐私边界**：不写任何请求头，故 Authorization/Cookie/各类 token 一律不落盘（回归测试有断言）。**接入范围**：11 个渠道的 ChatStream 全部接上——workbuddy / workbuddyai / qodercn / qoder / qodercom / qwenwork / traework / raccoon / loomy / monkeycode / glm，每处接 transport error 与 HTTP ≥400 两条路径。**验证**：`internal/capture` 6 条测试全绿（异常落盘/传输层失败/显式关闭零写盘/超包截断/文件名不覆盖/取模型名容错）；`internal/upstream` 用 `TestChatStreamCapturesOnlyOnError` 替换 R49 的旧测试（新语义：正常 200 零写盘 + 400 必落盘且含双边原文）。**杀伤力三验**：①`Save` 恒返回空 → 4 条 FAIL；②uid 不落盘 → 2 条 FAIL；③不截断超包 → FAIL；④移除 upstream 400 分支的 Save 调用 → `TestChatStreamCapturesOnlyOnError` FAIL「实际 0（取证未接入）」。**生产验证**：部署 PID 24968 后，正常请求 HTTP 200 且捕获目录文件数不变（6→6），故意请求不存在模型 → HTTP 400 且新增 `err-...-workbuddy-400-131.json`，内容含 uid=196316fc、model、131 字节出站体（含 `stream:true` 改写）、750 字节上游 11102 原文，无任何 token。 | **教训一（最重要）**：测试全绿 ≠ 测试有效。我第一次做「uid 不落盘」变异时，替换字符串缩进没对上（实际是 4 个 tab，我写了 3 个），`str.replace` 静默不生效，测试照样 PASS——**差点把假断言当成验证通过**。教训：变异脚本必须先 `assert anchor in src` 再替换。 **教训二**：给 11 个渠道批量插代码时，「按上下文找 return 再插入」的通用模板会踩变量名差异（qodercn/qoder/qodercom 的请求体是 `encoded string`、qwenwork 是 `prepared []byte`、traework 无中间变量、monkeycode/glm 没有 `Classify`）。第一版脚本把整个文件的 `kind` 都改了，编译报 8 处 undefined，只能 `git checkout` 全部重来。**逐个文件手改 + 每改一个就 `go build ./internal/<pkg>/`** 才是正确节奏。 **教训三**：`Classify` 返回 `provider.ErrKind` 不是 string，写进 `Record.Classify`（string）必须 `fmt.Sprint` 包一层。 |
| R55 | qodercn 深排队「每次重试都从头排」：①熔断 key 命名空间错位——Stream 写熔断用 handler 传入的 clientModel（带 `qodercn/` 前缀与 ` (0.00x)` 费率后缀），ChatStream 读熔断用 rewriteModel 后的裸模型名，生产熔断 0 次命中（17:21:30 记 5630、17 秒后照样打上游拿 9176）→ fuseKey 读写两侧统一归一化；②R46 fail-fast 对交互式客户端不友好（上游位次不保留，重投=重新排）→ ChatStream 保活循环：预算 5min、按上游 retry_after 节奏（夹 5–60s）整体重投，三形态全覆盖（HTTP≥400+parseQueueBody / 200 流内首帧扫描 classifyLeadFrame 攒字节丢弃重投 / 首帧看门狗与 75s 帧间静默按 hang 处理），预算耗尽原样放行交下游出 error 帧；③熔断固定 10min 窗被保活每跳续期恒 >预算 → 门口 10ms 硬拒把愿等的 DSH 轮次永久锁死（20:00 实锤 remain=578s>300s）→ 窗口动态化 fuseWindow=waitTime+60s 夹 [90s, 4min]，上界严格小于保活预算，入口熔断剩余在预算内不再门口干等、直接转保活节奏。教训：熔断读写两侧必须共用同一取键函数，跨命名空间回归测试（TestFuseKeyCrossNamespaceBlock/TestFuseWindowBoundedByHoldBudget）钉住；R46 测试两端手写同串把 key 错位挡在门外 |
| R56 | **等待期必须可感知：受理帧 + 实时面板 + 失败原因入统计行**（2026-10-23，用户「没反馈我只会觉得是卡住了」） | **事故**：15:51:50 `session route platform=qodercn` → 15:54:50 `stream relay end ... err=upstream stream truncated` + `access ... status=200 bytes=205 dur=3m0.044s`，中间 **180 秒 app.log 一行都没有**；客户端只收到 205 字节，`/api/live` 14 次采样全是 `phase=streaming attempts=0`。用户原话（paraphrased）：「等待可以，但没反馈；报错内容即使返回不了也要能在 7863 看到；没地方看然后一直计时我只会觉得是卡住了」。**根因**：①网关在等上游首内容期间零观测点；②TTFB 被写响应头的时间记成「已出流」，掩盖真实等待；③失败原因只在 app.log，面板「请求记录」看不到；④上游扁平错误帧（`{"code":"110","message":"Billing daily count exceeded"}`）虽被 `parseNestedSSE` 的扁平兜底下传，但面板仍显示「还在等首字」。**修法**：①`internal/live` 包 + `GET /api/live`（在飞请求表：阶段/前方位次/预计等待/第几跳/最近进展，2s 轮询）；②`stats.ReqRow` 增 `Err` 字段，六处错误出口全部写入一句话失败原因，面板「请求记录」加「失败原因」列 + `st-row-bad` 红底；③`provider.ProgressSink` 可选能力接口（**用 body 作键**，避免并发请求把 A 的排队挂到 B 的面板行），qodercn 七处上报点全覆盖；④`acceptWriter` 包装 ResponseWriter：**先设 SSE 响应头再发受理注释帧**，11 渠道统一覆盖；⑤`live.MarkContent` 单独记「首个内容时刻」，把「在等」与「在出内容」分开。**一轮修正（重要）**：最初在 handler 里直接 `io.WriteString(w, ...)` 发受理帧 → 响应头被永久定成 `Content-Type: text/plain; charset=utf-8`（SSE 客户端拒绝解析），且与 qodercn 内部那帧重复成两帧；改成 acceptWriter 统一负责、渠道层撤掉自己那帧。**验证**：受理帧 1.5s 内到达（原来 180s 内 0 字节），响应头 `text/event-stream` + `X-Accel-Buffering: no`，只发一次；`activity_test.go`/`acceptwriter_test.go`/`live_test.go`/`progress_test.go`/`live_visible_test.go` 全绿；杀伤力验证：删内容通知 / 关掉 `looksLikeErrorFrame` 分支各自必红。 | **教训一（最重要）**：**「以为修了」是本轮最大的坑**。我插入 `looksLikeErrorFrame` 分支后以为生效，实际那次 python 在 AssertionError 处中断、patch 根本没写进文件；测试照样 PASS 是因为 `parseNestedSSE` 的扁平兜底本来就下传。**判据**：写码后必须 `grep -c` 确认锚点命中数，变异验证必须**先确认变异真的落地**（`grep` 看到 `if false &&`）再跑测试。**教训二**：`io.WriteString(w, ...)` 会让 Go 隐式写出响应头——任何「先发一帧再设头」的写法都是错的，顺序必须是 Set 头 → 写 → flush。**教训三**：观测需求不要改共用的窄接口（`provider.Upstream` 有 11 个实现），用能力断言按需启用，缺省渠道行为不变。**教训四**：新增的受理帧让 TTFB 失去信息量，必须另记「首个内容时刻」，否则面板把「连接建立」当成「用户看到第一个字」。
| R57 | qodercn 排队重投节奏按上游 waitTime 而非固定 30s + GC 关停同步 | 用户 2026-10-23 20:08「快到了然后又重新开始了，可能要按他返回的时间等待才行」。生产日志（同一请求五跳）：20:05:58 前方4339/等待119s → 20:06:28 前方2138/59s → 20:06:58 前方4390/120s → 20:07:29 前方2348/65s → 20:07:59 前方4730/129s。名次「降一次、回队尾」锯齿 = 每 30s 固定重投一次，等于每 30s 主动放弃一次已排到的位次（上游按请求排队，不按账号留位），永远追不回来。修法：①holdPauseFor 按 waitTime 决定节奏并提前 ≤8s 重投（避免上游 180s 自己掐断带走位次）；②保活预算 5m→10m（覆盖实测 480s 高峰窗口再加两跳余量）；③熔断窗上界 4m→8m 并拆出 fuseMinWindow=30s（不再被 queueHangWindow 90s 抬高，waitTime=36s 时少封 54s）；④fuseWindow 余量 +60s→+30s；⑤入口熔断命中时用剩余时间收紧 waitTime，新请求不从头等一整轮。回归：TestHoldPauseFollowsUpstreamWaitTime（kill: 退回固定 30s 必红）、TestFuseWindowBoundedByHoldBudget（断言 queueBlockWindow<defaultQueueHoldBudget 上界护栏）。附带修掉 internal/session 的 StopGC 竞态：close(stop) 后不等 goroutine 退出，正在飞的 gcOnce 会删掉关停后写入的过期项（TestGCLifecycle 实测三次两红；旧注释记的 stop channel 竞态是 2026-08 的旧 bug，不覆盖本症状）。修法加 stopDone channel 同步等待，新增 TestStopGCIsSynchronous（200 轮放大窗口，kill 后第 60 轮必红）。 |
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
POST /api/account/checkin_all
POST /api/account/refresh          # {uid}
POST /api/account/refresh_all
POST /api/account/remove           # {uid}
POST /api/account/disable          # {uid,disabled} 停用/启用
POST /api/account/resource_detail  # {uid} → 积分明细
POST /api/account/nickname          # {uid,nickname} 修改显示名
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
> session 约 14 天，到期需重新导入；故 `RefreshToken` 留空、定时任务全关（保活会被
> 按「无 refresh token」跳过，但每天仍空跑一次并记失败）。鉴权失败是 **HTTP 200 +
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
17. **积分「可用/不可用」拆分统计**：`provider.ResourceItem.Usable` 标记条目是否属于本工具可消耗的额度池，`provider.Summarize()` 汇总小计。
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
18. **到期时间字段因渠道而异，缺失则不显示**：WorkBuddy 系是 `CycleEndTime`（**上游从不下发 `PackageEndTime`**，旧判据恒 miss），
    TraeWork 是 `expire_time`（Unix 秒），Qoder 无此字段。均按 **UTC+8 墙钟**解析（`softRateResetLoc`），
    用 `time.Local` 会在非 UTC+8 机器上算错一天。上游未下发时 `ResourceItem.ExpireAt` 必须为空串，
    前端据此隐藏整列——**不得用零值时间冒充「永不过期」**。
19. **401 必须自愈，不能只信本地 `expiresAt`**：上游刷新会作废旧 access token（refresh token 同步轮换）。若新 token 未落盘、
    或同一账号在别处被刷新，本地文件里的 token `expiresAt` 仍在未来，但上游已拒绝 → `NeedsRefresh` 恒为假、永不刷新、
    积分恒 0、明细恒空。因此积分/明细/费率路径遇 `ErrSessionDead` 必须「refresh + 落盘 + 重试一次」
    （`app.refreshIfSessionDead`，scheduler 的 checkin 路径同理）。
20. **凡是调 `Upstream.RefreshToken` 的地方必须紧跟 `SaveAtomic`**：refresh token 会轮换，不落盘 = 下次启动用旧 refresh token，
    重回上一条的死锁（`RefreshPricing` 曾漏，已补）。
21. **虚拟账号/单账号渠道不得进入任何「能把它弄没」的路径**：`reloadAccounts` 不纳入（`SyncToDir` 会剔除），
    启动时 `SetDisabled(uid,false)` 兜底自愈；`RemoveAccount`/`DisableAccount` 对此类账号硬拒（后端拒 + 前端无入口）。
    其 `Auth.ExpiresAt` 必须为远期值（不得为 0），否则 `NeedsRefresh` 恒真 → 反复 `RefreshToken` + 冷却。
    > 历史上唯一的无账号渠道是 OpenCodeZen（`oczen/*`），已于本项目移除。
    > 该渠道留下的两处机制仍在代码里，将来接入新的匿名通道可直接复用：
    > `Runtime.SingleAccount`（下条）与 `NO_CREDITS` 空集（前端「积分不适用」显示）。
22. **单账号渠道（`Runtime.SingleAccount`）不得施加任何账号级惩罚**：唯一账号且不可重登 ⇒ 任何惩罚
    （冷却/计数/禁用）都等于整条渠道下线。故 `Runtime.SingleAccount=true` 的渠道在 handler 里
    **传输层错误与 `status>=400` 一律原文透传**，不走 `NoteError`/`Cooldown`/`Disable`。
    - **429 也不冷却**：无号可轮换，冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，
      不如透传 429 让客户端按 `Retry-After` 退避。
    - **传输层错误不累计 `errCount`**：否则 3 次网络抖动（默认 `ErrThreshold`）即冷却唯一账号。
    - 启动时 `Pool.ClearPenalty(uid)` 自愈遗留冷却（`SetDisabled` 只清禁用，不清冷却）。
    - **严禁**把 401/403 归为 `ErrSessionDead`（会 `pool.Disable` 永久禁用且无法人工恢复）。
    - 该渠道的 `Classify` 仍做语义分类（供日志），但**不再用于决定惩罚**（惩罚判定前已短路）。
23. **Qoder 双区签到只能走 campaigns，且状态必须结构化上报**：
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

push `v*` tag → GitHub Actions 构建五平台产物并创建正式 release。
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
- [AGENTS.md](AGENTS.md) — 本文件：决议项（R1–R47）、架构选型、不变量
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
