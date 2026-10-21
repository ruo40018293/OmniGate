#!/usr/bin/env bash
# 构建发布用二进制。
#
# ## 为什么必须有这个脚本
#
# 发布用的构建与日常 `go build` 有两处**不可省略**的差异，漏掉任一项都会
# 把开发者本机的信息带进公开产物：
#
#   -trimpath   把编译时的绝对源码路径（$GOROOT、C:/Users/xxx/...）从二进制
#               里剥掉。Go 默认会把它们烧进 DWARF 与 panic 栈回溯里 ——
#               不加就是白送一份「你的用户名 + 工具链安装路径」。
#               实测不加时 exe 里有 13 处 C:/Users/Administrator/...。
#
#   -s -w       去符号表与 DWARF。顺带把体积从 14.6MB 压到 10.5MB，
#               对「下载即可用」的场景收益明显。
#
# 之前散落在 4 个 md 里的构建命令都没带这两个 flag，属于典型的
# 「文档写了但没人照做」隐患，故收敛到这一个脚本。
#
# ## 用法
#
#   bash scripts/build_release.sh              # Windows amd64（默认）
#   bash scripts/build_release.sh linux-amd64
#   bash scripts/build_release.sh darwin-arm64
#
# 环境要求：Go 在 PATH 中；build/windows/icon.ico 与
# cmd/omnigate/rsrc_windows_amd64.syso 已生成（见 build/ICON_README.md）。

set -euo pipefail

TARGET="${1:-windows-amd64}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

case "$TARGET" in
  windows-amd64)  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 ;;
  windows-arm64)  GOOS=windows GOARCH=arm64 CGO_ENABLED=0 ;;
  linux-amd64)    GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 ;;
  linux-arm64)    GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 ;;
  darwin-amd64)   GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 ;;
  darwin-arm64)   GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 ;;
  *) echo "未知目标: $TARGET" >&2; exit 2 ;;
esac

OUT="dist/omnigate"
case "$GOOS" in windows) OUT="${OUT}.exe" ;; esac

# Windows GUI 子系统：-H windowsgui 让它不弹控制台窗口。
# 只对 Windows 生效；Linux/macOS 用 -s -w 即可。
LDFLAGS="-s -w"
[ "$GOOS" = "windows" ] && LDFLAGS="-s -w -H windowsgui"

echo "构建 $GOOS/$GOARCH → $OUT"
echo "  ldflags: $LDFLAGS  (含 -trimpath，剥除本机绝对路径)"

mkdir -p dist
GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED="$CGO_ENABLED" \
  go build -trimpath -ldflags "$LDFLAGS" -o "$OUT" ./cmd/omnigate

SIZE=$(wc -c < "$OUT")
echo "完成: $OUT ($((SIZE / 1024 / 1024)) MB)"

# ── 发布前自检 ──────────────────────────────────────────────────
# 只在 Windows 产物上做（其余平台无 .rsrc 可查，但路径检查通用）。
fail=0
if grep -qa "C:/Users/" "$OUT"; then
  echo "!! 产物含本机绝对路径 C:/Users/... —— 检查是否漏了 -trimpath" >&2
  fail=1
fi
if grep -qaE "ghp_|gho_|github_pat_" "$OUT"; then
  echo "!! 产物含 GitHub token" >&2
  fail=1
fi
if [ "$fail" -ne 0 ]; then
  echo "自检未通过，不要发布。" >&2
  exit 1
fi
echo "自检通过：无本机路径、无 token。"