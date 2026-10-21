# v0.1.4

吸收上游两个参考项目（[wild-work](https://github.com/rockswang/wild-work)、[workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)）的更新，修复 5 个问题。

## 工具结果里的图片不再撑爆上下文与请求体

Codex 这类无状态客户端每轮重放整段历史，工具结果里的截图会以 `input_image` 块躺在 `function_call_output` 里。此前转换逻辑取不到文本就回退整块 JSON 序列化，于是整张 PNG 的 base64 变成一条超长**文本**消息——实测 1.6MB base64 约 146 万 token，1M 上下文的模型直接报 `prompt is too long`；即便绕过 token，12 张 2048px 截图累积成 33MiB 请求体也会撞上限回 413。

- 工具结果中的图片改走 vision 通道（`image_url` 内容块），纯文本仍按原样返回字符串；
- 图片独占工具结果时补一条 `[image]` 文本占位，避免部分渠道丢弃非 text 块后 content 变空；
- 转发前把过大的内联图片（data URL）缩到长边 ≤1568px 并重编码（不透明转 JPEG q85，带透明保持 PNG），按内容哈希缓存 256 条；`http(s)` 直链不动，解码失败/像素数离谱/体积没变小一律原样返回；
- 入站请求体上限 32MiB → **128MiB**，并新增配置项 `server.max_request_body_mb`（可配，钳制在 8~1024MiB，0 = 默认 128）。

## 整包到期不再被记成「今日消耗」

赠送包整包到期消失时，若该条目此前已被花掉几分，上游回包会呈现 `total` 下降、`used` 归零、`remaining` 下降。旧口径按「余额下降且无到期日」判定为消耗，面板于是显示一笔并不存在的花费。

现在用恒等式 `remain = total - used` 拆项：`total` 变小判定为池子缩水（记作到期作废），`used` 变大才是真消耗。判定设三道门槛——条目无到期日、该渠道被证实分别上报 total 与 used、total 确实下降且上次快照有 total——任一不满足都退回旧口径，对既有渠道零影响。

## 上游 5xx 默认不再计入账号错误

5xx 的语义是服务端故障，与账号凭据健康无关。旧口径下一次上游 500 风暴会让整池健康账号累计 `errCount`，达到阈值后全部冷却十分钟，整条渠道 503——而账号的积分、签到、session 一切正常。现在 5xx 默认不计入账号错误，确有需要逐渠道显式开启。

## 出站请求支持走环境变量代理

显式构造 `http.Transport` 会覆盖 Go 的默认代理行为，导致自建 Transport 的渠道永远不走代理（`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 全部失效）。现已在全部出站 Transport 上显式设置 `http.ProxyFromEnvironment`。

## 面板新增「今日作废」列

运行统计的平台表在「今日消耗」旁新增一列，显示因积分包到期/回收而作废的额度（即从今日消耗里扣掉的那部分），便于核对净值消耗的构成。

---

## 升级方式

1. 下载 `omnigate-windows-amd64.exe`，覆盖旧版即可（配置与账本数据兼容，无需迁移）。
2. macOS 用户下载 `WorkBuddyAPI-0.1.4-<arch>.dmg`。
3. Linux 用户下载对应架构的二进制。

## 新增配置项

```json
{
  "server": {
    "max_request_body_mb": 128
  }
}
```

省略或填 0 即为默认 128MiB。旧配置文件不含该字段时行为即为 128MiB。