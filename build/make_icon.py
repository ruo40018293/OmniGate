#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成 omnigate 的多尺寸 Windows 图标。

## 为什么不用一张图直接缩

原 `appicon.png`（858px 笔刷「W」）在 16/24px 下几乎无法辨识：
笔刷飞白细节全部消失、笔画粘连成绿块、圆角边缘因半透明过渡而发虚。
直接 LANCZOS 缩到 16px 得到的是「一团绿」，任务栏里认不出是哪个程序。

图标要在**小尺寸下也能认出来**，就不能只做减法 —— 细节该删的删、
骨架该加的加。所以这里按尺寸分档渲染，而不是一套图缩到底。

## 分档策略

| 档 | 尺寸 | 处理 |
|---|---|---|
| small | 16 / 20 / 24 | 纯骨架：加粗笔画、去掉飞白与内高光、圆角相对更大（视觉上补偿半透明边缘） |
| mid | 32 / 40 / 48 | 骨架 + 极轻的高光，无飞白 |
| large | 64 / 128 / 256 | 完整质感：渐变 + 飞白笔触 + 内高光 + 外阴影 |

笔画粗细按 `size * k` 给，k 随档位递减：小尺寸需要**相对更粗**才连得起来
（16px 下 0.15*16≈2.4px 才能形成连续笔画；大尺寸用 0.105 反而显得纤细好看）。

## 用法

    python build/make_icon.py

产出：
    build/windows/icon.ico     多尺寸 ico（exe 图标 + 托盘共用）
    build/icon_preview.png     各尺寸实际效果对照图（人工核查用）
"""

from __future__ import annotations

import math
import os

from PIL import Image, ImageDraw, ImageFilter

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
OUT_ICO = os.path.join(HERE, "windows", "icon.ico")
OUT_PREVIEW = os.path.join(HERE, "icon_preview.png")

# ── 配色 ───────────────────────────────────────────────────────────────
# 沿用原 appicon 的绿→青渐变，保持品牌连续性。
BG_TOP = (26, 30, 33)        # 深色圆角底
BG_BOTTOM = (15, 18, 20)
W_TOP = (110, 231, 92)       # W 上端亮绿
W_BOTTOM = (34, 209, 163)    # W 下端青
STROKE_TOP = (168, 255, 150) # 飞白高光
STROKE_BOTTOM = (86, 240, 205)

# ── 字形骨架 ───────────────────────────────────────────────────────────
# W 的折线骨架，归一化坐标（0~1）。
#
# 两个主笔画**外撇**（顶 0.235/0.765 宽于底 0.335/0.665）：这是手写体 W 的特征，
# 也是它在 16px 下比等宽机械 W 更好认的原因 —— 笔画像两支斜插的笔，
# 负空间（两个三角谷）比对称竖笔形成的空隙更醒目。
#
# 中峰 0.445 明显高于两侧顶 0.300：这是 W（而非 VV）的判别特征。
# 若中峰压得太低，两个谷会连成「双 V」，在小尺寸下读作 UU/VV 而不是 W。
# 反之也不是越高越好：过高会让中峰变成孤立的尖刺。
W_PATH = [
    (0.235, 0.300),
    (0.335, 0.700),
    (0.500, 0.445),
    (0.665, 0.700),
    (0.765, 0.295),
]

# 各尺寸的渲染档位参数：k=笔画粗细系数，r=圆角系数
TIERS = {
    16:  dict(k=0.170, r=0.235, hi=False, grain=False, shadow=False),
    20:  dict(k=0.160, r=0.230, hi=False, grain=False, shadow=False),
    24:  dict(k=0.150, r=0.225, hi=False, grain=False, shadow=False),
    32:  dict(k=0.135, r=0.220, hi=True, grain=False, shadow=False),
    40:  dict(k=0.122, r=0.218, hi=True, grain=False, shadow=False),
    48:  dict(k=0.114, r=0.215, hi=True, grain=False, shadow=True),
    64:  dict(k=0.108, r=0.212, hi=True, grain=False, shadow=True),
    128: dict(k=0.104, r=0.210, hi=True, grain=False, shadow=True),
    256: dict(k=0.102, r=0.208, hi=True, grain=False, shadow=True),
    # 512：供官网/文档 hero 用的大图。大尺寸下笔画相对更细才好看，
    # 且圆角比例可略降（绝对圆角已足够，比例太高会显得"幼稚"）。
    512: dict(k=0.098, r=0.200, hi=True, grain=False, shadow=True),
}

# 渲染时用 4 倍超采样再缩小：抗锯齿质量远好于直接在小画布上画。
SS = 4

# ico 里包含哪些尺寸：覆盖任务栏/托盘/资源管理器/开始菜单的常见请求
ICO_SIZES = [16, 20, 24, 32, 40, 48, 64, 128, 256]


def _lerp(a, b, t):
    return tuple(round(a[i] + (b[i] - a[i]) * t) for i in range(3))


def _vertical_gradient(size, top, bottom):
    """竖直线性渐变。用 numpy 之外的纯 Pillow 做法（一次横向拉伸）。"""
    grad = Image.new("RGB", (1, size))
    px = grad.load()
    for y in range(size):
        px[0, y] = _lerp(top, bottom, y / max(1, size - 1))
    return grad.resize((size, size), Image.BILINEAR)


def _draw_w(layer, size, cfg):
    """在 layer 上画 W：渐变填充 + 描边。

    做法是「先画粗折线做遮罩，再用渐变贴上去」，而不是直接画彩色线条 ——
    这样 W 才有从上到下的颜色渐变，而不是单一色。
    """
    k = cfg["k"]
    width = max(1, int(round(size * k)))

    # 归一化 → 像素
    pts = [(x * size, y * size) for x, y in W_PATH]

    mask = Image.new("L", (size, size), 0)
    md = ImageDraw.Draw(mask)
    md.line(pts, fill=255, width=width, joint="curve")
    # line 的端点是平的，圆头在小尺寸下会缺角；补两个圆头
    for x, y in (pts[0], pts[-1]):
        r = width / 2.0
        md.ellipse([x - r, y - r, x + r, y + r], fill=255)

    grad = _vertical_gradient(size, W_TOP, W_BOTTOM).convert("RGBA")
    layer.paste(grad, (0, 0), mask)
    return mask


def _add_grain(img, mask, cfg):
    """飞白：模拟笔锋扫过时露出的底色缺口。**当前所有档位默认关闭**。

    ## 保留实现但不用 —— 四版试出来的结论

    图标尺寸上限 256px，飞白这种细节在这个尺度上做不出毛笔质感，四版全部翻车，
    且失败模式各不相同：

    1. 沿笔画中线铺亮带 -> 在 W 中间拉出一条贯通全长的连续亮线，像画错了路径；
    2. 短促但**平行于**笔画 -> 成了条形码式的平行条纹；
    3. 改为**垂直于**笔画（几何上正确）-> 密度稍高就交织成「网格纹」，像织物；
    4. 降低密度与强度 -> 规则排列感仍在，大尺寸下像脏点。

    根因不在参数而在**尺度**：飞白依赖笔画足够宽才能呈现肌理，而 256px 下
    笔画只有约 26px 宽，无论怎么调密度都只是「噪点」而非「笔触」。要真正
    成立需把笔画加粗到 ~60px（巨图级别），而那时又因过粗而失去 W 的辨识度。

    补测 512px 巨图（笔画宽约 50px，理论上够宽了）：**仍是条纹**，
    因为缺口按笔画**等距**分布，等距本身就是「规则」的来源。
    要自然就得引入真随机 + 密度衰减，而那在图标这种要反复看的东西上
    只会显得脏。故最终所有档位（含 512）一律 grain=False。

    下面这份几何正确的实现保留着，供将来需要时复用（若届时配合随机化
    与密度衰减改造，可能在巨图上成立）。

    几何要点：缺口方向取笔画的**法向**（垂直于运笔方向），长度受笔画宽度限制，
    并沿笔画长度错开分布。只在笔画内部摆动，不溢出边界（最后统一用 W 遮罩裁剪）。
    """
    if not cfg["grain"]:
        return
    size = img.size[0]
    g = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    gd = ImageDraw.Draw(g)

    # 密度是这类质感的成败点。第三版按 size/16 撒缺口，256 下每段 16 个、
    # 缺口又密又短 —— 交织成「网格纹」，像织物而不是毛笔。
    # 正确取向：少而长。飞白本就是偶发的分叉，不是均匀纹理。
    n = max(3, int(size / 34))          # 每段笔画上的缺口数（256 → 7）
    lw = max(1, int(size * 0.011))      # 缺口自身的粗细（沿笔画方向）
    gap_len = size * cfg["k"] * 0.78    # 缺口长度（法向），约笔画的八成宽

    for si in range(len(W_PATH) - 1):
        x0, y0 = W_PATH[si][0] * size, W_PATH[si][1] * size
        x1, y1 = W_PATH[si + 1][0] * size, W_PATH[si + 1][1] * size
        dx, dy = x1 - x0, y1 - y0
        seglen = math.hypot(dx, dy)
        if seglen < 1:
            continue
        ux, uy = dx / seglen, dy / seglen
        nx, ny = -uy, ux                 # 法向

        for i in range(n):
            t = (i + 0.5) / n
            # 沿笔画方向稍微随机化，避免等距排列的机械感
            jitter = 0.18 * math.sin(i * 2.7 + si * 1.3)
            t = min(0.96, max(0.04, t + jitter * 0.12))
            cx = x0 + dx * t
            cy = y0 + dy * t
            # 缺口中心在笔画内左右微摆：一半偏一侧，制造不均匀
            swing = (0.5 + 0.5 * math.sin(i * 1.9 + si)) * 0.34 - 0.17
            ox = ux * (size * cfg["k"]) * swing
            oy = uy * (size * cfg["k"]) * swing
            cx += ox
            cy += oy

            half = gap_len * (0.45 + 0.55 * math.sin(i * 3.3 + si * 2.1)) * 0.5
            alpha = 58 + 44 * (0.5 + 0.5 * math.sin(i * 2.3 + si * 1.7))
            gd.line(
                [(cx - nx * half, cy - ny * half), (cx + nx * half, cy + ny * half)],
                fill=STROKE_TOP + (int(alpha),),
                width=lw,
            )

    # 裁进 W 里：飞白不允许溢出笔画边界
    g.putalpha(Image.composite(g.getchannel("A"), Image.new("L", (size, size), 0), mask))
    img.alpha_composite(g)


def _add_highlight(img, mask, cfg):
    """内高光：W 上缘一条极淡的亮边，增加体积感。中大尺寸启用。"""
    if not cfg["hi"]:
        return
    size = img.size[0]
    h = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    hd = ImageDraw.Draw(h)
    lw = max(1, int(size * 0.016))
    a = 46 if size >= 48 else 34
    hd.line([(x * size, y * size) for x, y in W_PATH],
            fill=(255, 255, 255, a), width=lw, joint="curve")
    h.putalpha(Image.composite(h.getchannel("A"), Image.new("L", (size, size), 0), mask))
    img.alpha_composite(h)


def render(size: int) -> Image.Image:
    """渲染单个尺寸的图标（RGBA）。"""
    cfg = TIERS[size]
    S = size * SS  # 超采样画布

    # ── 底：圆角方形 + 竖直渐变 ────────────────────────────────
    bg = _vertical_gradient(S, BG_TOP, BG_BOTTOM).convert("RGBA")
    r = int(S * cfg["r"])
    mask_bg = Image.new("L", (S, S), 0)
    ImageDraw.Draw(mask_bg).rounded_rectangle([0, 0, S - 1, S - 1], radius=r, fill=255)
    bg.putalpha(mask_bg)

    # ── 阴影（大尺寸）：W 投在底上的柔影 ─────────────────────────
    if cfg["shadow"]:
        sh = Image.new("RGBA", (S, S), (0, 0, 0, 0))
        sm = Image.new("L", (S, S), 0)
        _draw_w(sh, S, dict(cfg, k=cfg["k"] * 1.06))
        sh.putalpha(Image.composite(sh.getchannel("A").point(lambda v: v // 3),
                                    Image.new("L", (S, S), 0), mask_bg))
        sh = sh.filter(ImageFilter.GaussianBlur(S * 0.012))
        bg.alpha_composite(sh)

    # ── W ──────────────────────────────────────────────────────
    w_layer = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    w_mask = _draw_w(w_layer, S, cfg)
    _add_grain(w_layer, w_mask, cfg)
    _add_highlight(w_layer, w_mask, cfg)
    bg.alpha_composite(w_layer)

    # ── 降采样到目标尺寸 ────────────────────────────────────────
    return bg.resize((size, size), Image.LANCZOS)


def build_preview(images):
    """各尺寸实际效果对照图（1:1 + 4 倍最近邻放大）。

    放到深色背景上：任务栏/托盘通常是深色，若在浅背景上检查会漏掉
    「浅色笔画在浅底上看不清」这类问题。
    """
    pad = 10
    row1_h = 256
    row2_h = 4 * 64
    W = sum(images[s].width for s in sorted(images)) + pad * (len(images) + 1)
    H = row1_h + row2_h + pad * 3

    sheet = Image.new("RGBA", (W, H), (150, 150, 150, 255))
    x = pad
    for s in sorted(images):
        im = images[s]
        tile = Image.new("RGBA", (im.width, im.height), (32, 32, 32, 255))
        tile.alpha_composite(im)
        sheet.alpha_composite(tile, (x, pad))
        x += im.width + pad

    x = pad
    for s in sorted(images):
        if s > 64:
            x += 64 * 4 + pad
            continue
        im = images[s]
        tile = Image.new("RGBA", (im.width, im.height), (32, 32, 32, 255))
        tile.alpha_composite(im)
        sheet.alpha_composite(tile.resize((s * 4, s * 4), Image.NEAREST),
                              (x, pad * 2 + row1_h))
        x += s * 4 + pad
    return sheet


def main():
    images = {s: render(s) for s in ICO_SIZES}

    os.makedirs(os.path.dirname(OUT_ICO), exist_ok=True)
    # Pillow 写多尺寸 ico：传 sizes 会为每个尺寸存一份独立位图。
    images[256].save(OUT_ICO, format="ICO",
                     sizes=[(s, s) for s in ICO_SIZES])
    build_preview(images).convert("RGB").save(OUT_PREVIEW)

    sz = os.path.getsize(OUT_ICO)
    print(f"ico      {OUT_ICO}  ({sz} bytes, sizes={ICO_SIZES})")
    print(f"preview  {OUT_PREVIEW}")


if __name__ == "__main__":
    main()