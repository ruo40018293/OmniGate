#!/bin/sh
# 在**开发机**交叉编译容器内使用的二进制（不需要 Docker）。
#
# 用法（本脚本位于 docker/，会自动定位项目根目录）：
#     sh docker/build-binary.sh
#
# 产出：docker/omnigate   —— linux/amd64 静态二进制（CGO_ENABLED=0）
#
# 为什么在开发机编译：NAS/弱网环境里 go build 会卡在拉取依赖（GOPROXY 超时），
# 而交叉编译产物是纯静态的，丢进 alpine 即可直接跑，容器内零编译、零拉取。
set -e

cd "$(dirname "$0")/.."
ROOT="$(pwd)"
OUT="$ROOT/docker/omnigate"

echo "==> 项目根目录：$ROOT"
echo "==> 交叉编译 linux/amd64 ..."

# -trimpath：去掉编译机上的绝对路径，避免把本机目录结构带进产物。
# -s -w    ：去掉符号表与调试信息（容器内无需调试），体积约减 25%。
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w" -o "$OUT" ./cmd/omnigate

echo "==> 完成：$OUT"
ls -la "$OUT"

echo ""
echo "==> 校验 ELF 与关键特征"
# 用 od 读 magic（不依赖 file 命令，Windows/Git Bash 下也能用）
magic=$(head -c 4 "$OUT" | od -An -tx1 | tr -d ' \n')
if [ "$magic" = "7f454c46" ]; then
    echo "    ✓ ELF 头正确"
else
    echo "    ✗ 非 ELF（magic=$magic），请检查 GOOS/GOARCH"
    exit 1
fi

# 内嵌时区库自检：不内嵌时定时签到会在容器里静默按 UTC 触发。
if grep -qa "PST8PDT" "$OUT"; then
    echo "    ✓ 已内嵌时区库（含冷门时区 PST8PDT）"
else
    echo "    ✗ 未检测到时区库！请确认 main.go 里有 import _ \"time/tzdata\"，"
    echo "      否则容器内 TZ=Asia/Shanghai 不生效、定时签到时刻会偏移。"
    exit 1
fi

echo ""
echo "下一步（任选）："
echo "  A) 传整个 docker/ 目录到 NAS，执行 sh build-on-nas.sh 现场构建镜像"
echo "  B) 在有 Docker 的机器上执行 sh docker/build-image.sh 导出离线镜像 tar"
