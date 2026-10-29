#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""图标几何 / 对比度自检（不需要肉眼，也不需要 vision 模型）。

## 为什么需要它

`check_res.py` 验的是「exe 里的资源树对不对」，验不了「图标本身长什么样」。
而图标最容易出的两类问题又恰恰是看不见的：

- **不对称**：拱的圆心偏 1px，大尺寸下就是「门歪了」；
- **小尺寸糊掉**：16px 下门槛与门框糊成一坨，形状退化成「小写 n」。

这两类问题都能量化，所以做成断言而不是靠截图比对。判据都带出处：

| 判据 | 取值 | 依据 |
|---|---|---|
| 左右对称 | 差 ≤ 1px | 256px 下 1px 偏移肉眼可见，2px 就明显歪 |
| 门槛比门框宽 | 16px ≥ 1px，≥32px ≥ 3px | 门槛是「门」与「小写 n」的判别特征，没了形状就错 |
| 笔画与底对比 | 相对亮度比 ≥ 4.5:1 | WCAG AA 对大号图形的要求（图标按图形元素算） |
| 安全边距 | ≥ 4% 画布 | macOS/Windows 都给图标留四周视觉边距，贴边会被裁 |

## 用法

    python build/check_icon.py

退出码非 0 表示有断言失败，别发布。
"""

from __future__ import annotations

import os
import sys

from PIL import Image

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import make_icon  # noqa: E402

SIZES = [16, 20, 24, 32, 40, 48, 64, 128, 256]

FAILS: list[str] = []


def _rel_lum(rgb) -> float:
    def ch(v):
        v = v / 255.0
        return v / 12.92 if v <= 0.03928 else ((v + 0.055) / 1.055) ** 2.4
    r, g, b = rgb[:3]
    return 0.2126 * ch(r) + 0.7152 * ch(g) + 0.0722 * ch(b)


def _contrast(a, b) -> float:
    la, lb = _rel_lum(a), _rel_lum(b)
    hi, lo = max(la, lb), min(la, lb)
    return (hi + 0.05) / (lo + 0.05)


def _mark_mask(im: Image.Image) -> Image.Image:
    """把「底」和「门」分开：门是亮绿青，底是近乎中性的深灰。

    判定用 g - max(r, b)：底 r≈g≈b（≈0），门 g 明显高于 r/b（>60）。
    """
    px = im.convert("RGBA").load()
    w, h = im.size
    m = Image.new("L", (w, h), 0)
    mp = m.load()
    for y in range(h):
        for x in range(w):
            r, g, b, a = px[x, y]
            if a > 40 and (g - max(r, b)) > 24:
                mp[x, y] = 255
    return m


def check_size(size: int) -> None:
    im = make_icon.render(size)
    m = _mark_mask(im)

    # ── 1. 左右对称 ──
    l, t, r, b = m.getbbox() or (0, 0, 0, 0)
    left, right = l, size - r
    if abs(left - right) > 1:
        FAILS.append(f"{size}px 左右不对称：左边距 {left} / 右边距 {right}")
    top, bottom = t, size - b
    if abs(top - bottom) > max(2, round(size * 0.06)):
        FAILS.append(f"{size}px 上下留白失衡：上 {top} / 下 {bottom}")

    # ── 2. 安全边距 ──
    if min(left, right, top, bottom) < size * 0.04:
        FAILS.append(f"{size}px 边距过小：{min(left, right, top, bottom)}px")

    # ── 3. 门槛比门框宽（判别特征）──
    px = m.load()
    sill_y = int(make_icon.SILL_Y * size)
    sill_y = min(size - 1, max(0, sill_y))
    sill_run = _run_at(px, size, sill_y)
    # 门框宽度取「拱腿中段」那一行的亮段总长（左右各一段）
    leg_y = int((make_icon.SPRING_Y + make_icon.FEET_Y) / 2 * size)
    leg_total = _total_bright(px, size, min(size - 1, max(0, leg_y)))
    need = 1 if size < 32 else 3
    if sill_run - leg_total < need:
        FAILS.append(
            f"{size}px 门槛不够突出：门槛 {sill_run}px vs 门框 {leg_total}px"
            f"（至少多 {need}px）")

    # ── 4. 对比度 ──
    # 笔画取拱顶中心列里**最亮**的那一行：小尺寸下笔画落在哪一行随超采样相位浮动，
    # 写死一个 y 会取到门洞内部（那里只有很淡的透光），从而误报「对比度 1.0」。
    rgba = im.convert("RGBA")
    y0 = int((make_icon.SPRING_Y - make_icon.R) * size) - 2
    y1 = int(make_icon.SPRING_Y * size) + 2
    stroke = max((rgba.getpixel((size // 2, y)) for y in range(max(0, y0), min(size, y1))),
                 key=lambda c: 0.299 * c[0] + 0.587 * c[1] + 0.114 * c[2])
    # 底取四角内侧的平均（圆角处是透明的，取 12% 内缩避开）
    corners = [rgba.getpixel((int(size * 0.12), int(size * 0.88))),
               rgba.getpixel((int(size * 0.88), int(size * 0.88))),
               rgba.getpixel((int(size * 0.12), int(size * 0.12))),
               rgba.getpixel((int(size * 0.88), int(size * 0.12)))]
    bg_avg = tuple(sum(c[i] for c in corners) // 4 for i in range(3))
    ratio = _contrast(stroke, bg_avg)
    if ratio < 4.5:
        FAILS.append(f"{size}px 对比度不足：{ratio:.1f}:1（要求 ≥4.5:1）")

    print(f"  {size:>3}px  边距 {min(left, right, top, bottom)}px  "
          f"门槛 {sill_run} vs 门框 {leg_total}  对比 {ratio:.1f}:1")


def _run_at(px, size, y):
    """该行最长连续亮段长度。"""
    best = cur = 0
    for x in range(size):
        if px[x, y]:
            cur += 1
            best = max(best, cur)
        else:
            cur = 0
    return best


def _total_bright(px, size, y):
    return sum(1 for x in range(size) if px[x, y])


def main() -> int:
    print("图标几何自检：")
    for s in SIZES:
        check_size(s)
    if FAILS:
        print("\n失败：")
        for f in FAILS:
            print("  [FAIL] " + f)
        return 1
    print("\n全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
