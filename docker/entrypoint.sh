#!/bin/sh
# omnigate 容器入口（无头模式）。
#
# 职责：
#   1. 由 WILDWORK_* 环境变量在容器内生成 config.json
#      （配合 WILDWORK_HOME=/app，二进制把工作目录定在此处并读到这份配置）。
#      config.json 只存在于容器内，**永不 bind 挂载**，从根上避免
#      「单文件挂载被 Docker 建成立目录」导致的崩溃。
#   2. 对「对外监听 + 未设管理密码」这一**会被程序拒绝启动**的组合，
#      提前给出中文可读提示（否则用户只能看到一行 fatal）。
#   3. 以 --no-tray 启动（容器内没有桌面环境）。
set -e

# json_escape：转义 \ " 以及换行/制表符。
# 路径或密钥含特殊字符时若不转义，会生成非法 JSON，程序启动即崩，排查成本很高。
json_escape() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\r\n\t'
}

HOME_DIR="${WILDWORK_HOME:-/app}"
LISTEN="${WILDWORK_LISTEN:-:7863}"
API_KEY="${WILDWORK_API_KEY:-}"
ADMIN_PASSWORD="${WILDWORK_ADMIN_PASSWORD:-}"
AUTH_DIR="${WILDWORK_AUTH_DIR:-/data/auths}"
STATE_FILE="${WILDWORK_STATE_FILE:-/data/state.json}"
REGION="${WILDWORK_REGION:-cn}"
TIMEOUT="${WILDWORK_TIMEOUT_SECONDS:-120}"
HARD_CREDIT="${WILDWORK_HARD_CREDIT:-12h}"
SOFT_RATE="${WILDWORK_SOFT_RATE:-60s}"
ERR_THRESHOLD="${WILDWORK_ERR_THRESHOLD:-3}"
ERR_COOLDOWN="${WILDWORK_ERR_COOLDOWN:-10m}"
GROWTH_REDEEM="${WILDWORK_GROWTH_REDEEM_STREAK:-false}"
GROWTH_TRAVEL="${WILDWORK_GROWTH_TRAVEL:-false}"
GROWTH_TRAVEL_LOC="${WILDWORK_GROWTH_TRAVEL_LOCATION:-1}"

# to_int <值> <兜底>：只保留数字；清洗后为空则用兜底。
# 防止把 "abc" 之类的值直接写进 JSON 造成解析失败。
to_int() {
    v=$(printf '%s' "$1" | tr -cd '0-9')
    [ -z "$v" ] && v="$2"
    printf '%s' "$v"
}

# is_true：宽松识别布尔环境变量（1/true/yes/on，大小写不敏感）。
is_true() {
    case "$(printf '%s' "$1" | tr 'A-Z' 'a-z')" in
        1 | true | yes | on) return 0 ;;
        *) return 1 ;;
    esac
}

# is_loopback：监听地址是否「仅本机可访问」。
# 语义必须与 Go 侧 config.Listen.IsLoopback 保持一致：
# **只有显式写环回地址/localhost 才算安全；空主机名（":7863"）一律视为对外暴露。**
is_loopback() {
    case "$LISTEN" in
        localhost* | 127.* | "[::1]"* | ::1*) return 0 ;;
        *) return 1 ;;
    esac
}

ERR_THRESHOLD=$(to_int "$ERR_THRESHOLD" 3)
TIMEOUT=$(to_int "$TIMEOUT" 120)
GROWTH_TRAVEL_LOC=$(to_int "$GROWTH_TRAVEL_LOC" 1)

mkdir -p "$HOME_DIR" "$AUTH_DIR"
mkdir -p "$(dirname "$STATE_FILE")"

# ---------- 生成 config.json（只写必要字段；未写字段由程序用默认值补齐） ----------
cat > "$HOME_DIR/config.json" <<EOF
{
  "listen": "$(json_escape "$LISTEN")",
  "api_key": "$(json_escape "$API_KEY")",
  "admin_password": "$(json_escape "$ADMIN_PASSWORD")",
  "auth_dir": "$(json_escape "$AUTH_DIR")",
  "state_file": "$(json_escape "$STATE_FILE")",
  "region": "$(json_escape "$REGION")",
  "cooldown": {
    "hard_credit": "$(json_escape "$HARD_CREDIT")",
    "soft_rate": "$(json_escape "$SOFT_RATE")",
    "err_threshold": ${ERR_THRESHOLD},
    "err_cooldown": "$(json_escape "$ERR_COOLDOWN")"
  },
  "upstream": {
    "timeout_seconds": ${TIMEOUT}
  },
  "schedule": {
    "growth_redeem_streak": $(is_true "$GROWTH_REDEEM" && echo true || echo false),
    "growth_travel": $(is_true "$GROWTH_TRAVEL" && echo true || echo false),
    "growth_travel_location": ${GROWTH_TRAVEL_LOC}
  }
}
EOF

echo "[entrypoint] 已生成 $HOME_DIR/config.json"
echo "[entrypoint]   listen=${LISTEN}  region=${REGION}  timeout=${TIMEOUT}s"
echo "[entrypoint]   auth_dir=${AUTH_DIR}"
echo "[entrypoint]   state_file=${STATE_FILE}"
if [ -z "$API_KEY" ]; then
    echo "[entrypoint]   ⚠️  API_KEY 为空：OpenAI 兼容接口无鉴权！仅限本机/内网，切勿暴露公网"
else
    echo "[entrypoint]   api_key=<已设置>"
fi
if [ -n "$ADMIN_PASSWORD" ]; then
    echo "[entrypoint]   admin_password=<已设置>（Web 面板需登录）"
elif is_loopback; then
    echo "[entrypoint]   admin_password 为空（仅本机监听，面板不鉴权）"
else
    echo "[entrypoint]   ✗ 无法启动：监听 ${LISTEN} 已对外暴露，但没有设置 WILDWORK_ADMIN_PASSWORD。"
    echo "[entrypoint]     程序会拒绝启动 —— 这是为了避免局域网内任意设备无凭据打开管理面板"
    echo "[entrypoint]     （面板可增删账号、退出进程）。请在 compose 里设置 WILDWORK_ADMIN_PASSWORD 后重启。"
    exit 1
fi
if [ -n "$GROWTH_REDEEM$GROWTH_TRAVEL" ] && { is_true "$GROWTH_REDEEM" || is_true "$GROWTH_TRAVEL"; }; then
    echo "[entrypoint]   ⚠️  成长自动化已开启（兑换抽奖=${GROWTH_REDEEM} 猫猫旅行=${GROWTH_TRAVEL}）："
    echo "[entrypoint]     这些动作会**真实改变账号状态**（消耗抽奖次数不可撤销），请确认已知悉。"
fi

exec /app/omnigate --no-tray
