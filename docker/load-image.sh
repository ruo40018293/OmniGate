#!/bin/sh
# ============================================================================
# omnigate 离线镜像一键导入（在飞牛 NAS 或任意装了 Docker 的 Linux 上执行）
#
# 用法：
#   sh load-image.sh                       # 自动查找当前目录下的镜像 tar
#   sh load-image.sh /path/to/xxx.tar      # 指定 tar 路径
#
# 做的事：
#   1. 校验 tar 的 SHA256（若同目录存在 SHA256SUMS）
#   2. docker load 导入镜像
#   3. 打印镜像信息与后续步骤
#
# 为什么走离线导入：NAS 上现场构建需要 Dockerfile + 二进制，
# 且若让 compose 去构建/拉取基础镜像，弱网环境很容易卡在拉取上。
# 离线 tar 自带全部层，完全不需要网络。
# ============================================================================
set -e

TAR="$1"
SUMS="SHA256SUMS"

# ---------- 0. 自动发现镜像文件 ----------
if [ -z "$TAR" ]; then
    for f in omnigate-*-linux-amd64.tar omnigate-*.tar; do
        [ -f "$f" ] && TAR="$f" && break
    done
fi

if [ -z "$TAR" ] || [ ! -f "$TAR" ]; then
    echo "✗ 找不到镜像文件。"
    echo "  请把 omnigate-<版本>-linux-amd64.tar 放到当前目录，"
    echo "  或作为第一个参数指定路径：sh load-image.sh /path/to/xxx.tar"
    exit 1
fi

echo "==> 镜像文件：$TAR ($(du -h "$TAR" | cut -f1))"

# ---------- 1. 校验 SHA256 ----------
if [ -f "$SUMS" ]; then
    echo "==> 校验 SHA256 ..."
    if command -v sha256sum >/dev/null 2>&1; then
        # SHA256SUMS 里可能记录多个文件；只校验存在的那个。
        if sha256sum -c "$SUMS" 2>/dev/null | grep -F "$TAR"; then
            echo "    ✓ 校验通过"
        else
            echo "    ✗ 校验失败（或 SUMS 中无此文件记录）！"
            echo "      文件可能损坏或被篡改，请重新拷贝。"
            exit 1
        fi
    else
        echo "    (无 sha256sum，跳过校验)"
    fi
else
    echo "==> 未找到 $SUMS，跳过校验"
fi

# ---------- 2. 导入 ----------
echo "==> docker load 导入镜像（约需 10~30 秒）..."
docker load -i "$TAR"

# ---------- 3. 结果 ----------
echo ""
echo "============================================================"
docker images | grep -E "REPOSITORY|omnigate" || true
echo "============================================================"
echo ""
echo "✓ 导入完成。接下来："
echo ""
echo "  1) 准备数据目录（改成你自己的路径）："
echo "       mkdir -p /vol1/appdata/omnigate"
echo ""
echo "  2) 把 fnos-compose.yml 里标 ★必改 的三处改好，然后启动："
echo "       docker compose -f fnos-compose.yml up -d"
echo ""
echo "  3) 看日志（确认 listen / auth_dir / 端口就绪）："
echo "       docker logs -f omnigate"
echo ""
echo "  4) 打开管理面板添加账号（浏览器，不是容器内 CLI）："
echo "       http://<NAS_IP>:7863/     ← 用 WILDWORK_ADMIN_PASSWORD 登录"
echo ""
echo "  5) 验证接口："
echo "       curl http://127.0.0.1:7863/healthz"
echo ""
echo "  说明：账号登录在 Web 面板内完成（支持 WorkBuddy / TraeWork / Qoder /"
echo "        QwenWork / 小浣熊 等），容器内不需要任何登录 CLI 助手。"
