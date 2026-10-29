#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成 OmniGate 的多尺寸图标（exe / 托盘 / 前端 logo / macOS 源图）。

## 标志：一道门

`OmniGate` = 多路上游账号聚合成**一个** OpenAI 兼容入口。客户端永远只看见
一道门，门后面有多少账号是工具的事 —— 所以标志取「门」而不是字母缩写：

    ┌──┐        圆拱门洞（一段上圆弧 + 两条竖边框）
    │  │
    │  │        门槛（一条横线，两端比门框各宽出一点）
  ──┴──┴──

### 为什么不是旧版的 W

旧图标是手写体 W（WorkBuddy 首字母），改名 OmniGate 后这个字母**不再成立**，
留着等于把旧品牌焊在新名字旁边。而换名字母（O / G）又会和一堆系统图标撞形，
「门」既贴合「Gateway」的语义，又是字母表之外的形状。

### 为什么 16px 还能认出来

整枚标志只有**两种笔画**：一段拱（圆弧 + 两竖）和一条横门槛。没有内部细节、
没有渐变过渡、没有描边套描边。小尺寸下唯一会丢的是字重，所以小档位把笔画
按比例加粗（见 TIERS 的 k），而不是靠缩放大图。

**门槛那一条横是判别特征**：只有拱没有门槛时，16px 下会读成小写 n / h；
加上门槛立刻变成「门洞 + 地面」，和任何字母都区分得开。这与旧版 W 用
「中峰抬高」区分 VV 是同一个思路 —— 小尺寸靠一个明确的额外特征取胜。

### 分档渲染，而不是一套图缩到底

| 档 | 尺寸 | 处理 |
|---|---|---|
| small | 16 / 20 / 24 | 纯骨架：加粗笔画、门洞相对更宽、无内部透光 |
| mid | 32 / 40 | 骨架 + 极轻的内高光 |
| large | 48 / 64 / 128 / 256 / 512 / 1024 | 完整质感：投影 + 门洞透光 + 内高光 |

### 为什么不做纹理（沿用上一版结论）

飞白/纹理这类细节在 256px 上限的图标尺度上做不出肌理：四版试下来分别变成
连续亮线、条形码、网格纹、脏点。根因是笔画只有二三十像素宽，任何「纹理」
都只是噪点。故一律不做颗粒，只保留大档位的投影与透光这两种**低频**明暗。

## 用法

    python build/make_icon.py

产出（都在仓库内，随源码提交）：

    build/windows/icon.ico     多尺寸 ico（exe 图标；CI 用它再生成 syso）
    build/icon_preview.png     各尺寸实际效果对照图（人工核查用）
    build/appicon.png          1024px 源图（macOS 打包时 sips 派生 icns）
    build/logo64.png           64px（前端标题栏 logo 源）

生成后还要同步两份消费方副本（脚本不自动做，避免本地构建的目录副作用）：

    cp build/windows/icon.ico  cmd/omnigate/build/trayicon.ico   # 托盘（go:embed）
    cp build/logo64.png        cmd/omnigate/web/logo64.png       # Web UI
    python build/make_syso.py                                    # exe 资源
"""

from __future__ import annotations

import os

from PIL import Image, ImageDraw, ImageFilter

HERE = os.path.dirname(os.path.abspath(__file__))
OUT_ICO = os.path.join(HERE, "windows", "icon.ico")
OUT_PREVIEW = os.path.join(HERE, "icon_preview.png")
OUT_APPICON = os.path.join(HERE, "appicon.png")
OUT_LOGO64 = os.path.join(HERE, "logo64.png")

# ── 配色 ───────────────────────────────────────────────────────────────
# 深色圆角底 + 绿→青渐变，沿用旧品牌的主色，让老用户仍能一眼认出是同一个工具。
BG_TOP = (26, 30, 33)
BG_BOTTOM = (15, 18, 20)
MARK_TOP = (110, 231, 92)     # 门上端亮绿
MARK_BOTTOM = (34, 209, 163)  # 门下端青
GATE_LIGHT = (120, 232, 165)  # 门洞里透出来的光（只在大档位）
GATE_GLOW_ALPHA = 0.20        # 透光强度：实测折算后门洞内绿通道 ≈60（底 ≈19），
                             # 够看出「门后有光」但不至于填成一块实色

# ── 几何骨架（归一化 0~1，画布即正方形） ─────────────────────────────────
# 拱的半径 R 与拱心高度 SPRING_Y 决定门洞比例：R=0.235 时门洞宽占画布 47%，
# 是「能塞进细节但小尺寸仍透气」的折中值。FEET_Y 是门框落地高度，
# SILL_Y 是门槛所在高度（比 FEET_Y 略低，形成一条独立的地面线）。
CX = 0.5
R = 0.235
SPRING_Y = 0.455      # 拱与两条竖边的切点高度；拱顶 = SPRING_Y - R = 0.220
FEET_Y = 0.720        # 门框竖边下缘
SILL_Y = 0.775        # 门槛横线
SILL_HALF = 0.335     # 门槛半宽：0.165~0.835，比门框（0.265/0.735）各宽出 0.070
                      # 小尺寸下这 0.07 只剩 1px 出头，所以不再往回收——它是「门」
                      # 与「小写 n」的唯一判别特征，宁可大一点

# 各档位参数：k=笔画粗细系数，r=圆角系数，其余是「这一档才开」的质感开关。
TIERS = {
    16:   dict(k=0.166, r=0.235, hi=False, light=False, shadow=False),
    20:   dict(k=0.158, r=0.231, hi=False, light=False, shadow=False),
    24:   dict(k=0.149, r=0.227, hi=False, light=False, shadow=False),
    32:   dict(k=0.136, r=0.223, hi=True,  light=False, shadow=False),
    40:   dict(k=0.126, r=0.220, hi=True,  light=False, shadow=False),
    48:   dict(k=0.118, r=0.217, hi=True,  light=True,  shadow=True),
    64:   dict(k=0.111, r=0.214, hi=True,  light=True,  shadow=True),
    128:  dict(k=0.104, r=0.211, hi=True,  light=True,  shadow=True),
    256:  dict(k=0.099, r=0.208, hi=True,  light=True,  shadow=True),
    # 512/1024：文档 hero 与 macOS 源图。大尺寸下笔画相对更细才好看。
    512:  dict(k=0.094, r=0.201, hi=True,  light=True,  shadow=True),
    1024: dict(k=0.090, r=0.197, hi=True,  light=True,  shadow=True),
}

# ico 里包含哪些尺寸：覆盖任务栏/托盘/资源管理器/开始菜单的常见请求
ICO_SIZES = [16, 20, 24, 32, 40, 48, 64, 128, 256]

# 渲染时用 4 倍超采样再缩小：抗锯齿质量远好于直接在小画布上画。
SS = 4


def _lerp(a, b, t):
    return tuple(round(a[i] + (b[i] - a[i]) * t) for i in range(3))


def _vertical_gradient(size, top, bottom):
    """竖直线性渐变。用纯 Pillow 做法（一次横向拉伸），不依赖 numpy。"""
    grad = Image.new("RGB", (1, size))
    px = grad.load()
    for y in range(size):
        px[0, y] = _lerp(top, bottom, y / max(1, size - 1))
    return grad.resize((size, size), Image.BILINEAR)


def _geom(S):
    """把归一化骨架换算成像素。"""
    w = max(1, int(round(S * 0)))  # 占位，实际笔宽在 _draw_mark 里按档位算
    cx = CX * S
    r = R * S
    spring = SPRING_Y * S
    feet = FEET_Y * S
    sill = SILL_Y * S
    sill_half = SILL_HALF * S
    del w
    return cx, r, spring, feet, sill, sill_half


def _draw_mark(layer, S, cfg):
    """在 layer 上画「门」：渐变填充 + 圆角笔端。

    做法是「先用蒙版画骨架，再用渐变贴上去」，这样笔画有从上到下的颜色过渡，
    而不是单一颜色；同时蒙版可以直接复用给阴影、透光裁剪，不必重画。
    """
    width = max(1, int(round(S * cfg["k"])))
    cx, r, spring, feet, sill, sill_half = _geom(S)

    mask = Image.new("L", (S, S), 0)
    md = ImageDraw.Draw(mask)

    # 拱：把椭圆盒向外扩 width/2，让描边的**中心线**落在半径 r 上
    half = width / 2.0
    box = [cx - r - half, spring - r - half, cx + r + half, spring + r + half]
    md.arc(box, 180, 360, fill=255, width=width)

    # 两条竖边：从拱的切点落到门槛上方
    for x in (cx - r, cx + r):
        md.line([(x, spring), (x, feet)], fill=255, width=width)

    # 门槛：比门框更宽的一条地面线，是 16px 下的判别特征
    md.line([(cx - sill_half, sill), (cx + sill_half, sill)],
            fill=255, width=width)

    grad = _vertical_gradient(S, MARK_TOP, MARK_BOTTOM).convert("RGBA")
    layer.paste(grad, (0, 0), mask)
    return mask


def _add_light(img, mask, cfg):
    """门洞透光：门后透出的一层柔光，暗示「门后面还有东西」。

    只在大档位开。做法是在门洞内填一层自下而上的渐变光，再用拱形内区裁剪 ——
    蒙版用「圆角矩形只圆上两个角」，正好等于门洞内缘。
    """
    if not cfg["light"]:
        return
    S = img.size[0]
    width = max(1, int(round(S * cfg["k"])))
    cx, r, spring, feet, _, _ = _geom(S)
    half = width / 2.0

    inner_x0, inner_x1 = cx - r + half, cx + r - half
    if inner_x1 - inner_x0 < 2:
        return

    glow = _vertical_gradient(S, MARK_BOTTOM, GATE_LIGHT).convert("RGBA")
    # 底部亮、顶部暗：光束从门槛往上收
    inner = Image.new("L", (S, S), 0)
    ImageDraw.Draw(inner).rounded_rectangle(
        [inner_x0, spring - (r - half), inner_x1, feet],
        radius=(r - half), fill=255, corners=(True, True, False, False))

    alpha = Image.new("L", (S, S), 0)
    alpha.paste(inner.point(lambda v: int(v * GATE_GLOW_ALPHA)), (0, 0), inner)
    glow.putalpha(alpha)
    img.alpha_composite(glow)


def _add_highlight(img, mask, cfg):
    """内高光：沿骨架画一条更细的亮线，稍微上移，形成左上受光。"""
    if not cfg["hi"]:
        return
    S = img.size[0]
    width = max(1, int(round(S * cfg["k"] * 0.30)))
    cx, r, spring, feet, sill, sill_half = _geom(S)
    shift = 0  # 不位移：位移会让细高光贴着笔画内缘，在大尺寸下显成一道接缝

    h = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    hd = ImageDraw.Draw(h)
    a = 52 if S >= 192 else 38
    half = width / 2.0
    box = [cx - r - half, spring - r - half - shift,
           cx + r + half, spring + r + half - shift]
    hd.arc(box, 180, 360, fill=(255, 255, 255, a), width=width)
    for x in (cx - r, cx + r):
        hd.line([(x - shift, spring - shift), (x - shift, feet - shift)],
                fill=(255, 255, 255, a), width=width)

    h.putalpha(Image.composite(h.getchannel("A"), Image.new("L", (S, S), 0), mask))
    img.alpha_composite(h)


def render(size: int) -> Image.Image:
    """渲染单个尺寸的图标（RGBA）。"""
    cfg = TIERS[size]
    S = size * SS

    # ── 底：圆角方形 + 竖直渐变 ────────────────────────────────
    bg = _vertical_gradient(S, BG_TOP, BG_BOTTOM).convert("RGBA")
    r = int(S * cfg["r"])
    mask_bg = Image.new("L", (S, S), 0)
    ImageDraw.Draw(mask_bg).rounded_rectangle([0, 0, S - 1, S - 1], radius=r, fill=255)
    bg.putalpha(mask_bg)

    # ── 投影（大档位）：门投在底上的柔影 ────────────────────────
    if cfg["shadow"]:
        sh = Image.new("RGBA", (S, S), (0, 0, 0, 0))
        _draw_mark(sh, S, dict(cfg, k=cfg["k"] * 1.02))
        sh.putalpha(Image.composite(sh.getchannel("A").point(lambda v: v // 3),
                                    Image.new("L", (S, S), 0), mask_bg))
        sh = sh.filter(ImageFilter.GaussianBlur(S * 0.014))
        bg.alpha_composite(sh)

    # ── 门 ─────────────────────────────────────────────────────
    mark = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    mark_mask = _draw_mark(mark, S, cfg)
    _add_light(mark, mark_mask, cfg)
    _add_highlight(mark, mark_mask, cfg)
    bg.alpha_composite(mark)

    return bg.resize((size, size), Image.LANCZOS)


def build_preview(images):
    """各尺寸实际效果对照图（上行 1:1，下行 ≤64 的 4 倍最近邻放大）。

    底用中性灰：任务栏/托盘通常是深色，浅底检查会漏掉「深色笔画在深底上看不清」
    这类问题，纯黑底又会掩盖圆角边缘的过渡问题。
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
    # Pillow 写多尺寸 ico：传 sizes 会为每个尺寸存一份独立位图
    images[256].save(OUT_ICO, format="ICO", sizes=[(s, s) for s in ICO_SIZES])
    build_preview(images).convert("RGB").save(OUT_PREVIEW)

    render(1024).convert("RGBA").save(OUT_APPICON)
    render(64).convert("RGBA").save(OUT_LOGO64)

    for path in (OUT_ICO, OUT_PREVIEW, OUT_APPICON, OUT_LOGO64):
        print(f"{os.path.relpath(path, os.path.dirname(HERE))}  "
              f"({os.path.getsize(path)} bytes)")
    print("下一步：cp 到 cmd/omnigate/{build/trayicon.ico,web/logo64.png}，"
          "再 python build/make_syso.py")


if __name__ == "__main__":
    main()
