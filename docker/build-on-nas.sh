#!/bin/sh
# 在 NAS 上现场构建 omnigate 镜像（路径 B）。
#
# 用法（把整个 docker/ 目录传到 NAS 后，在该目录内执行）：
#     sh build-on-nas.sh
#
# 前提：本目录下必须有 Dockerfile、entrypoint.sh 与 linux/amd64 二进制 omnigate。
# （二进制由开发机执行 build-binary.sh 交叉编译后，随 docker/ 目录一起传上来。）
set -e

cd "$(dirname "$0")"

IMAGE="${IMAGE:-omnigate:latest}"

echo "==> 检查所需文件"
missing=0
for f in Dockerfile entrypoint.sh omnigate; do
    if [ -f "$f" ]; then
        printf "  OK       %s\n" "$f"
    else
        printf "  缺失     %s\n" "$f"
        missing=1
    fi
done
if [ "$missing" = "1" ]; then
    echo ""
    echo "错误：缺少文件，无法构建。请确认整个 docker/ 目录都已上传，"
    echo "      且二进制已由开发机交叉编译（sh build-binary.sh）。"
    exit 1
fi

echo ""
echo "==> 检查二进制是否为 Linux ELF（magic 应为 7f454c46）"
magic=$(head -c 4 omnigate | od -An -tx1 | tr -d ' \n')
if [ "$magic" != "7f454c46" ]; then
    echo "  ✗ omnigate 不是 ELF 文件（magic=$magic）"
    echo "    可能传错了文件，或传输过程损坏 —— 请在开发机重新交叉编译后再传。"
    exit 1
fi
echo "  OK       omnigate (ELF)"

echo ""
echo "==> 检查 entrypoint.sh 的换行符"
# CRLF 会让容器内的 /bin/sh 报 "not found" 或语法错误，这是跨平台传文件最常见的坑。
if grep -q "$(printf '\r')" entrypoint.sh 2>/dev/null; then
    echo "  检测到 CRLF，自动转换为 LF ..."
    sed -i 's/\r$//' entrypoint.sh
    echo "  OK       已转为 LF"
else
    echo "  OK       LF"
fi

echo ""
echo "==> 构建镜像 $IMAGE"
docker build -t "$IMAGE" .

echo ""
echo "==> 完成。镜像信息："
docker images "$IMAGE"

echo ""
echo "下一步："
echo "  1) 把 fnos-compose.yml 标 ★必改 的三处改好，然后在「Docker → Compose」里创建项目"
echo "     （或直接：docker compose -f fnos-compose.yml up -d）"
echo "  2) 浏览器打开 http://<NAS_IP>:7863/ 添加账号（登录在面板内完成）"
echo "  3) 重启容器：docker compose -f fnos-compose.yml restart"
