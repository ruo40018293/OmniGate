# Docker 部署

把 omnigate 跑在容器里（飞牛 NAS / 任意 Linux）。**容器内以无头模式运行**（`--no-tray`），
管理与账号登录全部通过 Web 面板完成，不需要容器里的 CLI 助手。

---

## 一、为什么这样设计

这几条每一条都对应一次真实的部署失败：

| 设计 | 规避的问题 |
|---|---|
| 镜像只 COPY **预编译静态二进制**，容器内不编译 | NAS 上 `go mod download` 访问 proxy.golang.org 超时（`i/o timeout`） |
| 配置由 `entrypoint.sh` 在容器内生成 `config.json` | 把 `config.json` 作为**单文件** bind 挂载时，宿主缺该文件 → Docker 建出同名**空目录** → 程序读配置即崩（`Exited:0`） |
| 只挂载 `/data` 目录 | 同上；数据（账号 + 状态）持久化，可随镜像升级 |
| 二进制内嵌 tzdata | 精简镜像没有 `/usr/share/zoneinfo`，`TZ=Asia/Shanghai` 无法解析 → `time.Local` 静默退化为 **UTC** → 签到在北京时间 17:00/05:00 触发 |
| `entrypoint` 提前校验管理密码 | 对外监听 + 无密码时程序会 `fatal` 退出，提前给出中文提示比一行错误更好排查 |

---

## 二、两种部署路径

### 路径 A · 离线镜像（推荐，无需网络与构建）

1. 把 `omnigate-v<版本>-linux-amd64.tar` 与 `load-image.sh` 传到 NAS 同一目录
2. 在该目录执行一次：`sh load-image.sh`
3. 用 `fnos-compose.yml` 建容器（改掉里面 3 处 ★必改）

离线 tar 自带全部镜像层，**全程不需要网络**。

### 路径 B · NAS 现场构建

1. 开发机执行 `sh docker/build-binary.sh` 交叉编译出 `docker/omnigate`
2. 把整个 `docker/` 目录传到 NAS
3. 在 NAS 上 `sh build-on-nas.sh` → `docker build` 出镜像

### 路径 C · 有 Docker 的开发机打 tar

```sh
sh docker/build-image.sh          # 构建 + 导出 tar + SHA256SUMS 到 docker/dist/
```

---

## 三、目录说明

| 文件 | 作用 |
|---|---|
| `Dockerfile` | alpine 基础镜像；零 apk 依赖，只拷二进制与 entrypoint |
| `entrypoint.sh` | `WILDWORK_*` → 容器内 `config.json`；启动前校验；`exec --no-tray` |
| `docker-compose.yml` | NAS 上 `docker compose up -d --build` 用的构建版 |
| `fnos-compose.yml` | fnOS 图形界面粘贴用的版本（含 ★必改 标注与注释） |
| `build-binary.sh` | **开发机**：交叉编译 `linux/amd64` 二进制（含 ELF 与时区库自检） |
| `build-image.sh` | **有 Docker 的机器**：构建镜像 + 导出离线 tar + SHA256SUMS |
| `build-on-nas.sh` | **NAS 上**：现场构建镜像（含 ELF 与 CRLF 检查） |
| `load-image.sh` | **NAS 上**：校验 SHA256 + `docker load` 导入离线镜像 |

---

## 四、环境变量

### 必填（不填会启动失败或存在安全风险）

| 变量 | 说明 |
|---|---|
| `WILDWORK_ADMIN_PASSWORD` | Web 面板登录口令。**对外监听时必填**：程序拒绝在「对外暴露 + 无密码」下启动，防止局域网任意设备无凭据打开面板（可增删账号、退出进程）。 |
| `WILDWORK_API_KEY` | OpenAI 兼容接口的 Bearer 密钥。留空 = 接口无鉴权，任何人可消耗你的账号额度。 |

### 常用

| 变量 | 默认 | 说明 |
|---|---|---|
| `WILDWORK_LISTEN` | `:7863` | 容器内必须监听全部网卡（写 `127.0.0.1` 宿主访问不到） |
| `WILDWORK_AUTH_DIR` | `/data/auths` | 账号目录 |
| `WILDWORK_STATE_FILE` | `/data/state.json` | 冷却/签到状态 |
| `WILDWORK_REGION` | `cn` | 区域，只接受 `cn` |
| `TZ` | `Asia/Shanghai` | 影响定时签到触发时刻（已内嵌 tzdata，一定生效） |

### 可选

| 变量 | 默认 | 说明 |
|---|---|---|
| `WILDWORK_HARD_CREDIT` | `12h` | 余额/权益不足的冷却时长 |
| `WILDWORK_SOFT_RATE` | `60s` | 被限流的冷却时长 |
| `WILDWORK_ERR_THRESHOLD` | `3` | 累计错误几次进入冷却 |
| `WILDWORK_ERR_COOLDOWN` | `10m` | 错误冷却时长 |
| `WILDWORK_TIMEOUT_SECONDS` | `120` | 上游请求超时 |
| `WILDWORK_GROWTH_REDEEM_STREAK` | `false` | 连登档位自动兑换 + 抽奖次数自动抽完 |
| `WILDWORK_GROWTH_TRAVEL` | `false` | 猫猫旅行自动派出 + 到站领取 |
| `WILDWORK_GROWTH_TRAVEL_LOCATION` | `1` | 旅行目的地（1~4） |

> **成长自动化默认全部关闭**：它们会真实改变账号状态（兑换消耗连登档位、抽奖次数用掉不可撤销）。
> 建议先在面板里手动触发一两次，确认结果符合预期再开启自动执行。

---

## 五、添加账号

**在浏览器里做，不在容器里。** 打开 `http://<NAS_IP>:7863/`，
用 `WILDWORK_ADMIN_PASSWORD` 登录，在面板里发起登录：

| 类型 | 渠道 |
|---|---|
| 面板内完成 OAuth 轮询 | WorkBuddy、WorkBuddy AI、TraeWork、Qoder（三个变体）、QwenWork、小浣熊 |
| 需从本机官方客户端导入凭据 | MonkeyCode、Loomy |
| 需从浏览器 Cookie 粘贴令牌 | 智谱清言 GLM |

> 注：OpenCodeZen（匿名免费通道）已停用，面板不显示该渠道。

> ⚠️ 导入型渠道（MonkeyCode / Loomy）依赖**本机已登录的官方客户端**，容器里取不到这类凭据。

### ⚠️ 凭据是平台绑定的（重要，别踩）

Windows 上生成的 `auths/*.json` 里，token 是 **DPAPI 密文**（`dpapi:` 前缀），
密文与「当前 Windows 用户」绑定，**在 Linux / 容器里无法解密**。

| 方向 | 能否直接拷贝 auths/ |
|---|---|
| Windows → Linux / 容器 | ❌ **不能**。密文无法解密，账号会加载失败，日志打印「检测到 Windows DPAPI 密文，当前平台无法解密」 |
| Linux / 容器 → Windows | ✅ 可以。Linux 侧不做加密（明文 + 文件权限保护） |

所以**不要**把 Windows 上跑出来的 auths 目录拷进容器 —— 请在容器所在环境
**用管理面板重新登录一次**（面板地址 `http://<NAS_IP>:7863/`）。

这是刻意的设计而不是缺陷：DPAPI 的密钥在 Windows 用户配置里，
跨平台「解密」在密码学上不可能；Linux 侧若自造固定密钥加密，
只是制造安全假象，强度还不如直接依赖文件权限。

---

## 六、验证

```sh
docker logs -f omnigate                    # 看 entrypoint 打印的生效配置
curl http://127.0.0.1:7863/healthz              # 应为 ok
curl -H "Authorization: Bearer $API_KEY" http://127.0.0.1:7863/v1/models
```

`docker logs` 开头会打印一行 `[entrypoint] 已生成 /app/config.json` 及
`listen / auth_dir / state_file / api_key / admin_password` 的实际取值 ——
排查「配置到底生效了没有」先看这里。

---

## 七、从更早的部署迁移

早期版本曾用过 `WB2A_*` 环境变量前缀、`/admin/` 面板路径与独立登录助手二进制，
现已全部收敛。当前形态：

| 项 | 早期 | 现在 |
|---|---|---|
| 环境变量前缀 | `WB2A_*` | **`WILDWORK_*`** |
| 二进制数量 | 3 个（server + 2 个登录助手） | **1 个**（登录走 Web 面板） |
| 面板路径 | `/admin/` | **`/`** |
| 面板鉴权 | `WB2A_ADMIN=on/off` 开关 | `WILDWORK_ADMIN_PASSWORD`（对外监听必填） |
| 时区 | 依赖系统 zoneinfo | **二进制内嵌 tzdata** |

迁移时环境变量名必须**全部替换**；镜像名与容器名也相应调整，两者可并存（端口需错开）。
