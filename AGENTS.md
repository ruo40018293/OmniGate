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
| R61 | raccoon 无领取端点：每日 300 分靠「当天首次触达」懒发放，签到 = 摸一次余额 + 核对当日流水 |
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
- [AGENTS.md](AGENTS.md) — 本文件：决议项一句话判据（R1–R79）、架构选型、不变量
- [docs/DECISIONS.md](docs/DECISIONS.md) — 决议全文（R1–R79）：每条决议的原始背景、踩坑记录与边界条件
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
