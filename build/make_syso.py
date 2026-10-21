#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 .ico 转成 Go 工具链能自动链接的 COFF 资源文件（.syso）。

## 为什么不用 rsrc / windres

- `akavel/rsrc` 要 `go install`，本机 GOPROXY 不稳（此前踩过超时）；
- `windres`（binutils）本机没有，也属外部依赖。

ICO -> COFF 的转换不复杂，手写一个反而有几个好处：
**零外部依赖、可复现、注释能讲清 PE 资源布局**（这正是排查嵌入问题时最需要的知识）。

## 产物格式

Go 的链接器会扫描主包目录下的 `*.syso`，把它们当作目标文件里的
COFF 对象链接进去。因此这里生成的是一个标准的 amd64 COFF 文件，
内含一个 `.rsrc` 节，节里放标准的 IMAGE_RESOURCE_DIRECTORY 资源树，
其中 `RT_GROUP_ICON(14)` 指向 `RT_ICON(3)` 的各个尺寸条目。

    .syso (COFF)
     └── .rsrc 节
          ├── 根资源目录（type=RT_ICON, id=-1）
          │    └── name=3 目录 → 每个尺寸一个 IMAGE_RESOURCE_DATA_ENTRY → PNG 数据
          └── 根资源目录（type=RT_GROUP_ICON, id=1）
               └── name=-1 → IMAGE_RESOURCE_DIRECTORY_ENTRY → GRPICONDIR → 各尺寸 PNG 偏移表

## 关于 PNG 编码的小尺寸条目

Windows Vista 起支持在资源条目里直接放 PNG 数据（不用手工构造 BMP+调色板）。
本生成器**全部尺寸统一用 PNG**，简洁且无 alpha 损失。
注意：`RT_GROUP_ICON` 的 `GRPICONDIR` 里记录的是各条目在资源中的**逻辑偏移**
（0/1/2...），不是文件偏移 —— 这是 GROUP_ICON 与 ICON 的关键区别，写错则图标不显示。

## 用法

    python build/make_syso.py
    # 产出 cmd/omnigate/rsrc_windows_amd64.syso
"""

from __future__ import annotations

import os
import re
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
ICO = os.path.join(HERE, "windows", "icon.ico")
SYSO = os.path.join(ROOT, "cmd", "omnigate", "rsrc_windows_amd64.syso")

IMAGE_RESOURCE_TYPE_ICON = 3
IMAGE_RESOURCE_TYPE_GROUP_ICON = 14
IMAGE_RESOURCE_TYPE_VERSION = 16

# RT_VERSION 里放一个最小 VS_VERSIONINFO，让资源管理器能显示版本号。
# 不是必需，但没有它文件的「属性 - 详细信息」里产品名/版本会是空的。
VERSION_STRING = "omnigate"


# ── ICO 解析 ───────────────────────────────────────────────────────────
def parse_ico(path):
    """返回 [(w, h, png_bytes), ...]，按文件内顺序（即尺寸升序）。"""
    d = open(path, "rb").read()
    reserved, ico_type, count = struct.unpack("<HHH", d[:6])
    if reserved != 0 or ico_type != 1:
        raise SystemExit(f"{path} 不是 ICO（reserved={reserved} type={ico_type}）")
    out = []
    off = 6
    for _ in range(count):
        w, h, _c, _r, _pl, _bpp, size, offset = struct.unpack("<BBBBHHII", d[off:off + 16])
        off += 16
        # ICONDIR 的宽高为单字节，0 表示 256
        ww = w if w else 256
        hh = h if h else 256
        out.append((ww, hh, d[offset:offset + size]))
    return out


def dir_entry_size():
    """IMAGE_RESOURCE_DATA_ENTRY 大小：4 个 DWORD = 16 字节。"""
    return 16


def read_app_version():
    """从 internal/app/app.go 的 `const Version` 读取版本号。

    ## 为什么不能在这里写死

    版本号有两个必须一致的地方：

      1. 面板 logo 旁与「关于」显示的版本 —— 走 /api/state 下发 app.Version
      2. 文件属性（资源管理器 → 属性 → 详细信息）里的「文件版本」——
         来自 exe 内嵌的 RT_VERSION 资源，也就是本脚本生成的这段

    若第 2 处在这里写死（如曾经的 "1.0.0.0"），就会出现「面板写着 v0.1.2，
    文件属性却是 1.0.0.0」，且**没有任何地方会报错** —— 资源解析完全正常。
    单一来源是唯一的根治办法：读代码里的常量。
    """
    src = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                       "..", "internal", "app", "app.go")
    with open(src, "r", encoding="utf-8") as f:
        m = re.search(r'^const Version = "([^"]+)"', f.read(), re.M)
    if not m:
        raise SystemExit("✗ 在 internal/app/app.go 里找不到 `const Version = \"...\"`")
    return m.group(1)


def version_to_quad(ver):
    """'0.1.2' → (0, 1, 2, 0) 四元组。

    缺位补 0（'1.2' → 1.2.0.0）；多于 4 段直接报错而不是静默截断 ——
    静默截断会产出一个看起来正常、但版本号对不上的资源。
    """
    parts = ver.split(".")
    if len(parts) > 4:
        raise SystemExit("✗ 版本号段数过多（最多 4 段）：%r" % ver)
    nums = [int(p) for p in parts] + [0] * (4 - len(parts))
    for n in nums:
        if not 0 <= n <= 0xFFFF:
            raise SystemExit("✗ 版本号分段超出 DWORD 范围：%r" % ver)
    return tuple(nums)


def build_vs_versioninfo(name, version):
    """最小 VS_VERSIONINFO（UTF-16LE，固定长度字段）。"""
    def wstr(s):
        b = s.encode("utf-16-le")
        return struct.pack("<HH", len(b), len(b)) + b + b"\x00\x00"  # 含结尾 NUL

    a, b_, c, d = version_to_quad(version)
    ver_ms, ver_ls = (a << 16) | b_, (c << 16) | d
    # Windows 要求的四段式展示：'0.1.2' → '0.1.2.0'
    quad_text = "%d.%d.%d.%d" % (a, b_, c, d)

    # VS_FIXEDFILEINFO：13 个 DWORD（缺一个会让整个资源区解析失败，
    # 表现为「图标不显示」而非报错，极难定位）
    sig = 0xFEEF04BD
    ffi = struct.pack("<IIIIIIIIIIIII",
                      sig,                    # dwSignature
                      0x00010000,             # dwStrucVersion
                      ver_ms, ver_ls,         # dwFileVersionMS/LS
                      ver_ms, ver_ls,         # dwProductVersionMS/LS
                      0x3F,                   # dwFileFlagsMask
                      0x00,                   # dwFileFlags
                      0x00040004,             # dwFileOS = VOS__WINDOWS32
                      0x00010000,             # dwFileType = VFT_APP
                      0x00,                   # dwFileSubtype
                      0x00000000,             # dwFileDateMS
                      0x00000000)             # dwFileDateLS

    children = b""
    children += _version_block("FileDescription", "omnigate 账号聚合服务")
    children += _version_block("ProductName", name)
    children += _version_block("CompanyName", "")
    children += _version_block("LegalCopyright", "")
    children += _version_block("FileVersion", quad_text)
    children += _version_block("ProductVersion", quad_text)

    # 头部：wLength, wValueLength, wType, szKey="VS_VERSION_INFO", Value=ffi, Padding, Children
    key = "VS_VERSION_INFO".encode("utf-16-le") + b"\x00\x00"
    # 对齐到 4 字节
    total = 6 + len(key) + len(ffi) + 2 + len(children)
    total = (total + 3) & ~3
    hdr = struct.pack("<HHH", total, len(ffi), 0) + key
    hdr += b"\x00" * ((-len(hdr)) % 4)
    hdr += ffi
    hdr += b"\x00" * ((-len(hdr)) % 4)
    # Children 起始需 4 字节对齐
    hdr += b"\x00" * ((-len(hdr)) % 4)
    return hdr + children


def _version_block(key, value):
    k = key.encode("utf-16-le") + b"\x00\x00"
    v = value.encode("utf-16-le") + b"\x00\x00"
    total = 6 + len(k) + len(v)
    total = (total + 3) & ~3
    b = struct.pack("<HHH", total, len(v), 1) + k
    b += b"\x00" * ((-len(b)) % 4)
    b += v
    b += b"\x00" * ((-len(b)) % 4)
    return b


def align4(x):
    return (x + 3) & ~3


def build_syso(ico_path, syso_path):
    entries = parse_ico(ico_path)
    if not entries:
        raise SystemExit("ICO 里没有图像")

    # ---- 组装 .rsrc 节 ----
    n = len(entries)

    # ── 资源树布局规划 ──────────────────────────────────────────────
    #
    # PE 规范的资源树：
    #
    #   根目录
    #     └ type 条目（RT_ICON / RT_GROUP_ICON / RT_VERSION）
    #          └ id 条目                    ← 必需
    #               └ lang 条目             ← **必需**（中性语言 0）
    #                    └ 叶子 DATA_ENTRY
    #
    # ## 两个已踩过的坑（症状都是「构建成功、能启动、就是没图标」）
    #
    # ### 坑一：type 与 id 之间多插一层
    # 最初建成 `RT_ICON → id=1 → id=1..9`。后果：Windows 的
    # FindResource(RT_ICON, id=2) **找不到**（id=2 挂在更下一层，外层 id 恒为 1），
    # 而 GROUP_ICON 的 GRPICONDIRENTRY.nID 引用的是 1..9 → 整组失效。
    #
    # ### 坑二：漏掉 lang 层
    # 只建到 `type → id → 数据` 就收尾。Windows 的资源查找（SHGetFileInfo、
    # LoadIcon 等）**按语言 id 定位**，缺 lang 层时
    # SHGetFileInfoW 直接返回 FALSE + ERROR_NO_DATA(1008)，
    # 资源管理器因此不显示图标。lang 0 = 中性语言（"与语言无关"），最通用。
    #
    # 两个坑都表现为「自己的解析器看着没问题」（树非空、偏移在界内、PNG 合法），
    # 但 Windows 不认。故校验必须用**深度**判据 + Windows API 实测。

    def dir_size(k):
        return align4(16 + 8 * k)

    root = 0
    LANG_ID = 1033            # en-US：与系统 exe 一致（实测 regedit/explorer 都用它）

    root_size = dir_size(3)          # 3 个 type 条目

    # type 目录：RT_ICON 有 n 个 id，另两个各 1 个
    icon_type_dir = root + root_size
    group_type_dir = icon_type_dir + dir_size(n)
    ver_type_dir = group_type_dir + dir_size(1)

    # lang 层：**每个 id 一个独立的 lang 目录**。
    #
    # 为什么不能共用一个：资源目录项的 OffsetToData 指向「子目录的起点」，
    # 其后连续条目都属该子目录。若 9 个 id 全指向同一个 lang 目录，
    # 加载器会把整个目录读作每个 id 的子目录 → 每个 id 都遍历出 9 个叶子
    # （校验器报「RT_ICON id 应连续 1..9，实际重复 9 遍」）。
    #
    # 故这里给每个尺寸分配一个只含 1 个 lang 条目的独立目录。
    icon_lang_base = ver_type_dir + dir_size(1)

    # lang 目录偏移表：每个 id 一个**独立**目录（各 1 个 lang 条目），
    # 紧接其后是 GROUP_ICON 与 VERSION 的 lang 目录。
    icon_lang_dirs = [icon_lang_base + dir_size(1) * i for i in range(n)]
    group_lang_dir = icon_lang_base + dir_size(1) * n
    ver_lang_dir = group_lang_dir + dir_size(1)

    # 叶子：IMAGE_RESOURCE_DATA_ENTRY（各 16 字节），紧跟其后是实际数据
    #
    # 注意每个数据条目之后必须留出**它的数据长度**再放下一个条目，
    # 否则 GROUP_ICON 的 GRPICONDIR 会和 VERSION 的 VS_VERSIONINFO 重叠
    # （症状：RT_VERSION 的 OffsetToData 指向节外，版本信息读出乱码）。
    group_dataentry = ver_lang_dir + dir_size(1)
    grp_len = align4(6 + 14 * n)      # GRPICONDIR + n 个 GRPICONDIRENTRY(各14B)
    group_blob = group_dataentry + 16
    ver_data = group_blob + grp_len
    vs_info = build_vs_versioninfo(VERSION_STRING, read_app_version())
    ver_blob = ver_data + 16                     # DATA_ENTRY(16) 之后才是本体
    icon_dataentry = ver_blob + align4(len(vs_info))
    icon_blob = icon_dataentry + 16 * n

    png_offsets = []
    off = icon_blob
    for _w, _h, png in entries:
        off = align4(off)
        png_offsets.append(off)
        off += len(png)
    total = align4(off)

    buf = bytearray(total)

    # 记录每个 IMAGE_RESOURCE_DATA_ENTRY 里 OffsetToData 字段所在的**节内偏移**。
    # 这些 DWORD 必须参与重定位，否则链接器不会修正它们 —— 详见文件末尾说明。
    dataentry_fields = []

    def put(o, data):
        buf[o:o + len(data)] = data

    def directory(k):
        # Characteristics, TimeDateStamp, MajorVersion, MinorVersion,
        # NumberOfNamedEntries, NumberOfIdEntries
        return struct.pack("<IIHHHH", 0, 0, 0, 0, 0, k) + b"\x00" * (8 * k)

    def entry(name, is_dir, off_):
        # OffsetToData 最高位 = 1 → 子目录；= 0 → 叶子数据
        return struct.pack("<II", name, (0x80000000 if is_dir else 0) | off_)

    # 根目录 → 三个 type 目录
    put(root, directory(3))
    put(root + 16, entry(IMAGE_RESOURCE_TYPE_ICON, True, icon_type_dir))
    put(root + 16 + 8, entry(IMAGE_RESOURCE_TYPE_GROUP_ICON, True, group_type_dir))
    put(root + 16 + 16, entry(IMAGE_RESOURCE_TYPE_VERSION, True, ver_type_dir))

    # ── RT_ICON：type → id(1..n) → 各自独立的 lang(0) 目录 → DATA_ENTRY ──
    put(icon_type_dir, directory(n))
    for i in range(n):
        ld = icon_lang_dirs[i]
        put(icon_type_dir + 16 + 8 * i, entry(i + 1, True, ld))
        put(ld, directory(1))
        put(ld + 16, entry(LANG_ID, False, icon_dataentry + 16 * i))

    # ── RT_GROUP_ICON / RT_VERSION：type → id(1) → lang(0) → DATA_ENTRY
    put(group_type_dir, directory(1))
    put(group_type_dir + 16, entry(1, True, group_lang_dir))
    put(group_lang_dir, directory(1))
    put(group_lang_dir + 16, entry(LANG_ID, False, group_dataentry))

    put(ver_type_dir, directory(1))
    put(ver_type_dir + 16, entry(1, True, ver_lang_dir))
    put(ver_lang_dir, directory(1))
    put(ver_lang_dir + 16, entry(LANG_ID, False, ver_data))

    # GROUP_ICON 数据
    grp = struct.pack("<HHH", 0, 1, n)
    for i, (w, h, png) in enumerate(entries):
        # GRPICONDIRENTRY = 14 字节紧凑排列：
        #   BYTE  bWidth, bHeight, bColorCount, bReserved   (4)
        #   WORD  wPlanes, wBitCount                         (4)
        #   DWORD dwBytesInRes                               (4)
        #   WORD  nID                                       (2)
        #
        # 两个坑：
        #  1. nID 是 **WORD(2B)** 不是 DWORD。用 16 字节对齐格式会让每条目
        #     多 2 字节 → 整块偏移错位，Windows 解析出的尺寸与数量对不上。
        #  2. struct.pack 会按 4 字节对齐在 WORD 后补 padding，故先 pack 再
        #     [:14] 截断，得到紧凑的 14 字节。
        e16 = struct.pack("<BBBBHHIH",
                          w & 0xFF, h & 0xFF, 0, 0,   # w, h, bColorCount, bReserved
                          1, 32,                        # wPlanes, wBitCount
                          len(png), i + 1)              # dwBytesInRes, nID
        grp += e16[:14]
    put(group_dataentry, struct.pack("<IIII", group_blob, len(grp), 0, 0))
    dataentry_fields.append(group_dataentry)      # ← OffsetToData 字段位置
    put(group_blob, grp)

    # VERSION 数据
    #
    # 关键：DATA_ENTRY 头与 vs_info 本体是**两段独立区域**，不能重叠。
    # 早先写成 `put(ver_data, header)` 后紧接 `put(ver_data, vs_info)`，
    # 后者把前者整个覆盖 → OffsetToData 字段变成 vs_info 的头 4 字节
    # （0x0000349C 之类），Size 变成乱码 → 校验器报「越界 / 不是 PNG」。
    ver_blob = ver_data + 16
    put(ver_data, struct.pack("<IIII", ver_blob, len(vs_info), 0, 0))
    dataentry_fields.append(ver_data)
    put(ver_blob, vs_info)

    # ICON 数据条目 + PNG
    for i, (w, h, png) in enumerate(entries):
        put(icon_dataentry + 16 * i,
            struct.pack("<IIII", png_offsets[i], len(png), 0, 0))
        dataentry_fields.append(icon_dataentry + 16 * i)
        put(png_offsets[i], png)

# ---- 包装成 COFF ----
    section = bytes(buf)

    # ═══════════════════════════════════════════════════════════════════
    # 为什么必须有符号表 + 重定位表（这是本文件最关键的部分）
    #
    # Go 链接器 src/cmd/link/internal/ld/pe.go 的 addpersrc() 对每个
    # .rsrc 节做两件事：
    #
    #   1. 把节内容原样写入输出（ctxt.Out.Write(data)）
    #   2. 遍历 **该符号的重定位条目**，把每个指向的 DWORD 改写为
    #      新节的虚拟地址：
    #          val := uint32(int64(h.virtualAddress) + r.Add())
    #
    # 也就是说：资源目录里所有 IMAGE_RESOURCE_DATA_ENTRY.OffsetToData
    # 写的是**节内偏移**，必须靠重定位条目 + 节基址才能变成真实 RVA。
    #
    # 缺了重定位表 → 偏移保持节内相对值 → 资源目录里指针全错 →
    # **构建不报任何错，exe 也没有图标**（前几版就卡在这）。
    #
    # 权威对照：GOROOT/src/cmd/link/testdata/pe-binutils/rsrc_amd64.syso
    #   nReloc=1, PtrReloc=196, Characteristics=0xC0300040
    #   重定位条目 = {VirtualAddress=节内偏移, SymbolIndex=0, Type=3}
    #   Type 3 = IMAGE_REL_AMD64_32（对 4 字节 DWORD 加基址）
    #   符号 0 = {Name=".rsrc"(内联前8字节), Value=0, SectionNumber=1,
    #             Type=0, Class=3(STATIC), NumAuxSymbols=0}
    # ═══════════════════════════════════════════════════════════════════

    # ── 符号表：一个 STATIC 符号，名字是节名 ──────────────────────
    # 名字 ".rsrc" 只有 5 字节，**直接内联在符号的 Name 字段前 8 字节**
    # （COFF 符号名不足 8 字节时用全 8 字节存放，尾部必须补 NUL，
    #   Zeroes=0 且 StringTable=0 表示读的是内联名）。
    # 踩过的坑：只写 `sym[0:5] = b".rsrc"` 会让第 6 字节保持 0x00 之前的
    # 脏数据，解析出来变成 ".rsr" —— 链接器按名匹配失败，
    # 症状是构建成功但 exe 无图标。故整块清零后再写入。
    sym = bytearray(18)          # 全零：Zeroes=0, Value=0, StringTable=0,
    sym[0:5] = b".rsrc"           # Type=0, Class, NumAux=0
    sym[11] = 3                   # Class = IMAGE_SYM_CLASS_STATIC
    symbols = bytes(sym)

    # ── 重定位表：每个 OffsetToData DWORD 一条 ─────────────────────
    # IMAGE_RELOCATION: VirtualAddress(4) + SymbolTableIndex(4) + Type(2)
    IMAGE_REL_AMD64_32 = 3
    relocs = bytearray()
    for f in dataentry_fields:
        relocs += struct.pack("<IIH", f, 0, IMAGE_REL_AMD64_32)
    assert len(relocs) == 10 * len(dataentry_fields)

    # ── 文件布局 ────────────────────────────────────────────────────
    #   [0]  COFF 文件头 (20)
    #   [20] 节头 (40)
    #   [60] 节数据（4 字节对齐）
    #   [..] 重定位表
    #   [..] 符号表 (18)
    #   [..] 字符串表 (4)
    COFF_HDR_LEN = 20
    SEC_HDR_LEN = 40
    SYM_LEN = 18
    STRTAB_LEN = 4

    # PointerToSymbolTable / NumberOfSymbols 要在布局确定后回填，先占位
    coff_hdr = struct.pack("<HHIIIHH", 0x8664, 1, 0, 0, 0, 0, 0)
    assert len(coff_hdr) == COFF_HDR_LEN

    ptr_raw = align4(COFF_HDR_LEN + SEC_HDR_LEN)          # = 60
    ptr_reloc = ptr_raw + len(section)
    ptr_sym = ptr_reloc + len(relocs)

    # Characteristics 对齐官方样本：0xC0300040
    #   IMAGE_SCN_CNT_INITIALIZED_DATA (0x40)
    # | IMAGE_SCN_MEM_READ (0x40000000)
    # | IMAGE_SCN_MEM_WRITE (0x80000000)
    # 链接器不校验这个值，但与 binutils 产物保持一致更安全。
    CHARACTERISTICS = 0xC0300040

    sec_hdr = struct.pack("<8sIIIIIIHHI",
                          b".rsrc\x00\x00\x00",         # Name（必须正好是 .rsrc）
align4(len(section)),      # VirtualSize
                          0,                             # VirtualAddress（链接器重定位）
                          len(section),                  # SizeOfRawData
                          ptr_raw,                       # PointerToRawData
                          ptr_reloc if relocs else 0,      # PointerToRelocations
                          0,                             # PointerToLinenumbers
                          len(relocs) // 10 if relocs else 0,   # NumberOfRelocations
                          0,                             # NumberOfLinenumbers
                          CHARACTERISTICS)
    assert len(sec_hdr) == SEC_HDR_LEN, len(sec_hdr)

    strtab = struct.pack("<I", STRTAB_LEN)

    out = bytearray()
    out += coff_hdr
    out += sec_hdr
    assert len(out) == ptr_raw, (len(out), ptr_raw)
    out += section
    assert len(out) == ptr_reloc
    out += relocs
    assert len(out) == ptr_sym
    out += symbols
    out += strtab

    # 回填 COFF 头里的符号表位置
    coff_hdr = struct.pack("<HHIIIHH", 0x8664, 1, 0, ptr_sym, 1, 0, 0)
    out[0:COFF_HDR_LEN] = coff_hdr

    syso = bytes(out)

    os.makedirs(os.path.dirname(syso_path), exist_ok=True)
    open(syso_path, "wb").write(syso)
    return syso_path, len(entries), len(syso), sorted({w for w, _, _ in entries})


def main():
    path, n, size, dims = build_syso(ICO, SYSO)
    print(f"syso     {path}")
    print(f"         {n} 个尺寸 {dims}, {size} bytes")


if __name__ == "__main__":
    main()