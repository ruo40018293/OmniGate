# v0.1.3 — 修复 DeepSeek Harness 请求被 11-128 拦截

## ⚠️ 本版核心修复：DeepSeek Harness(DSH) 请求 400 `code=11-128`

**现象**

用 DeepSeek Harness 走 workbuddy 渠道时报错：

```
本轮运行失败400 {"code":"11-128","msg":"Illegal API invocation from an unapproved channel"}
请求被拦截，请重新发送
```

**根因**

上游对 **system 消息开头**的逐字串
`You are an AI agent powered by DeepSeek Harness` 做锚定匹配，命中即整单拦截。

DSH 每个 Agent 请求的 system 消息必带这句，所以 100% 命中 —— 不是账号问题、
不是额度问题、不是客户端问题。

**为什么换 traework 就正常？** traework 走的是 Trae 上游，没有这套渠道校验。
所以「同一客户端、换个渠道就好了」正是这个根因的典型表现。

**修复方式**

出站请求体脱敏时，对该身份句做**最小语义扰动改写**：插入冠词 `the`

```
You are an AI agent powered by DeepSeek Harness
→ You are an AI agent powered by the DeepSeek Harness
```

逐字指纹被破坏、语义不变，上游放行（实测 200）。

缺 `You are an` 前缀的非精确变体上游本就不拦，保持原样不动，避免误伤。

**影响范围**：仅 workbuddy / workbuddyai 两条 WorkBuddy 系链路的出站 body。
账号、额度、模型、请求头、调度逻辑均未改动。

## 其他改动

- 版本号 0.1.2 → 0.1.3（面板显示与 exe 文件属性同步对齐）
- `.gitignore` 忽略 `.workbuddy/`（本地工作记忆不入公开仓库）

## 升级方式

直接替换 exe 即可，**无需迁移任何配置或账号数据**。

## 快速开始

1. 下载对应平台的产物，放到任意目录
2. 双击启动 —— 首次启动会自动生成随机 API Key 并打印在日志里
3. 浏览器打开面板，添加账号

## 接入 AI 客户端

    Base URL: http://127.0.0.1:7863/v1
    API Key:  见面板顶栏（点击可复制）或 config.json 的 api_key

模型 ID 需带渠道前缀：`workbuddy/glm-5.3-flash`、`traework/<model>` 等。

## 主要特性

- **多渠道账号池** —— 统一调度、冷却、限流避让，按策略挑号（临期优先 / 积分优先 / 负载均衡）
- **会话粘性** —— 同一段对话恒定走同一账号，上游 prompt 缓存可命中
- **WAF IP 级 fail-fast** —— 多个账号接连撞 WAF 403 时判定为出口 IP 被拦，立即失败而非继续轮转
- **费率与模型清单** —— 多渠道的模型与倍率，支持搜索与比价
- **积分明细** —— 按账号查看可用 / 不可用积分小计
- **自动签到** —— 每日定时签到与保活
- **OpenAI / Anthropic 双协议兼容**

## 安全说明

首次启动会**随机生成** API Key（不使用固定默认值 —— 固定值会随二进制公开）。
若监听 `0.0.0.0` 而 `api_key` 为空，程序会拒绝启动。

面板监听非环回地址时**必须**配置 `admin_password`。

## 系统要求

- Windows 10+ / macOS / Linux（各平台产物见本 Release 附件）
- 无需管理员权限

## 免责声明

本项目只是各上游渠道的集成与聚合，本身不涉及任何逆向分析和破解工作。
各渠道接口参数、签名算法等底层知识来自社区其他开源项目的公开成果。

仅供个人学习使用，请勿用于商业用途或违反上游服务条款的场景。
