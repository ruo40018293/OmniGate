#!/bin/sh
# 在**装了 Docker 的机器**上执行：构建镜像并导出可直接导入的离线 tar。
#
# 用法（本脚本位于 docker/）：
#     sh docker/build-image.sh            # 版本号自动从源码读取
#     sh docker/build-image.sh 0.1.0      # 手动指定版本号
#
# 产出（写入 docker/dist/）：
#     omnigate-v<版本>-linux-amd64.tar   离线镜像（docker load 即可用）
#     SHA256SUMS                              校验和（load-image.sh 会用它校验）
#
# 产物用法：把 tar 与 load-image.sh 一起拷到 NAS，在 NAS 上 sh load-image.sh。
set -e

cd "$(dirname "$0")"
DOCKER_DIR="$(pwd)"
ROOT="$(cd .. && pwd)"

IMAGE="omnigate:latest"

# ---------- 0. 版本号：优先取参数，否则从 internal/app/app.go 的 const 读 ----------
VER="$1"
if [ -z "$VER" ]; then
    VER=$(grep -m1 'const Version = ' "$ROOT/internal/app/app.go" | sed -E 's/.*"([^"]+)".*/\1/')
fi
if [ -z "$VER" ]; then
    echo "✗ 无法确定版本号。请在命令行传入：sh docker/build-image.sh 0.1.0"
    exit 1
fi
echo "==> 版本号：$VER"

# ---------- 1. 确保二进制是最新的 ----------
if [ ! -f "$DOCKER_DIR/omnigate" ]; then
    echo "==> docker/omnigate 不存在，先交叉编译"
    sh "$DOCKER_DIR/build-binary.sh"
else
    echo "==> 检测到已存在的 docker/omnigate"
    echo "    ⚠️  如需确保是当前源码产物，请先删除它再运行本脚本（避免用到旧二进制）。"
    ls -la "$DOCKER_DIR/omnigate"
fi

# ---------- 2. 检查 Docker ----------
if ! command -v docker >/dev/null 2>&1; then
    echo ""
    echo "✗ 本机没有 docker 命令，无法构建镜像。"
    echo "  二选一："
    echo "   A) 在有 Docker 的机器上运行本脚本；"
    echo "   B) 传整个 docker/ 目录到 NAS，在 NAS 上执行 sh build-on-nas.sh 现场构建。"
    exit 1
fi

# ---------- 3. 构建 ----------
echo ""
echo "==> docker build -t $IMAGE"
docker build -t "$IMAGE" "$DOCKER_DIR"

# ---------- 4. 导出离线 tar ----------
mkdir -p "$DOCKER_DIR/dist"
TAR="$DOCKER_DIR/dist/omnigate-v${VER}-linux-amd64.tar"

echo ""
echo "==> docker save → $TAR"
docker save -o "$TAR" "$IMAGE"

echo ""
echo "==> 生成 SHA256SUMS"
( cd "$DOCKER_DIR/dist" && sha256sum "$(basename "$TAR")" > SHA256SUMS && cat SHA256SUMS )

echo ""
echo "============================================================"
ls -la "$DOCKER_DIR/dist/"
echo "============================================================"
echo ""
echo "✓ 完成。把 dist/ 里的 tar 与 SHA256SUMS，连同 load-image.sh，"
echo "  一起拷到 NAS 的同一个目录，然后在 NAS 上执行："
echo ""
echo "      sh load-image.sh"
echo ""
