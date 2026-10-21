#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""校验 .syso / exe 里的 Windows 资源是否正确。

## 为什么需要专门的校验脚本

嵌入图标这条链路上，**任何一步出错都不会让构建失败**：

    go build  → 成功（syso 被静默接受）
    运行 exe  → 正常启动（没有图标而已）
    解析 PE   → 要手工翻二进制才知道出了什么事

排查期间踩过的坑（每个都表现为「构建成功但没图标」）：
1. syso 节名写成 .rssec → 链接器按名识别失败，`.rsrc` 根本不生成；
2. PointerToRawData 漏算 40 字节节头 → 链接器把节头当节内容；
3. 缺符号表 / 缺重定位表 → 资源目录里的 OffsetToData 未被修正成 RVA，
   目录指针全错（这一版 syso 看起来完全正常，只有解析才能发现）。

所以这个脚本就是**判据本身**：构建后跑一次，PASS 才算真的成功。

用法:
    python build/check_res.py cmd/omnigate/rsrc_windows_amd64.syso
    python build/check_res.py dist/omnigate.exe
"""

from __future__ import annotations

import struct
import sys

RT_NAMES = {3: "RT_ICON", 14: "RT_GROUP_ICON", 16: "RT_VERSION", 24: "RT_MANIFEST"}
IMAGE_REL_AMD64_ADDR32NB = 0x0003


def _dir_at(buf, off):
    """IMAGE_RESOURCE_DIRECTORY → (named_count, id_count, first_entry_off)。

    加边界保护：exe 的 .rsrc 常紧邻文件末尾，损坏或越界的偏移会让
    切片不足 16 字节 → struct.unpack 直接抛异常。越界本身就是错误信号，
    这里按 0 条目返回，由上层报「资源树为空」，而不是抛出栈回溯。
    """
    if off < 0 or off + 16 > len(buf):
        return 0, 0, off + 16
    vals = struct.unpack("<IIHHHH", buf[off:off + 16])
    return vals[4], vals[5], off + 16


def _walk(buf, off, path=(), max_depth=4):
    """递归展开资源树，返回 [(path, dataentry_off)]，path 是 (type, id...) 路径。

    资源树是三层目录：根 → type 目录 → id 目录 → 叶子 DATA_ENTRY。
    手工逐层 `for` 容易把「目录自身的头」当成条目读（症状是条目数异常、
    OffsetToData 全 0），故这里用递归一次走到底 —— 与 Windows 加载器的
    遍历方式一致，也少一层出错机会。

    保留完整 path 是必需的：叶子 id 在不同 type 下会重复（都是 1），
    只看末级 id 无法区分 RT_GROUP_ICON 与 RT_VERSION 的数据。
    """
    out = []
    if len(path) > max_depth:
        return out
    n, i, ent = _dir_at(buf, off)
    if n + i > 4096:            # 明显异常的条目数，多半是读到了非目录
        return out
    for k in range(n + i):
        p = ent + k * 8
        if p + 8 > len(buf):
            break
        nid, o2 = struct.unpack("<II", buf[p:p + 8])
        if o2 & 0x80000000:
            out += _walk(buf, o2 & 0x7FFFFFFF, path + (nid,), max_depth)
        else:
            out.append((path + (nid,), o2 & 0x7FFFFFFF))
    return out


def _group_by_type(leaves):
    """[(path, off)] → {type_id: [off, ...]}（第一级 path 元素即 type）。"""
    by = {}
    for path, off in leaves:
        if path:
            by.setdefault(path[0], []).append(off)
    return by


def _check_tree_shape(leaves):
    """校验资源树层级是否正确 —— 这是最容易被漏掉、且症状最隐蔽的判据。

    **叶子路径必须恰好 3 层：(type, id, lang)。** 两个方向的错都会导致
    「构建成功、exe 能启动、资源管理器就是不显示图标」：

    1. **多了层**（`(type, 1, id)`）：Windows `FindResource(RT_ICON, id>1)`
       找不到 —— id 挂在更下一层；而 GROUP_ICON 的 `GRPICONDIRENTRY.nID`
       写的是 1..n，整组图标只有 id=1 能解析。
    2. **少了层**（`(type, id)`，没有 lang）：Windows 的资源查找**按语言 id
       定位**，缺 lang 层时 `SHGetFileInfoW` 直接返回 FALSE +
       `ERROR_NO_DATA(1008)`。lang 0 = 中性语言，资源管理器认这个。

    这两种都查不出「资源树非空」——树确实非空，只是形状不对。
    只有按**深度**判据才能发现。

    返回 (是否通过, 说明文字)。
    """
    ok = True
    depth_hist = {}
    for path, _lf in leaves:
        depth_hist[len(path)] = depth_hist.get(len(path), 0) + 1
    for path, _lf in leaves:
        if len(path) != 3:
            ok = False
            hint = ("少 lang 层 → SHGetFileInfo 报 1008" if len(path) < 3
                    else "多中间层 → FindResource 找不到 id")
            print(f"  FAIL: 叶子路径 {'/'.join(map(str, path))} 有 {len(path)} 层，"
                  f"应为 3 层 (type, id, lang) —— {hint}")

    # lang 层：实测系统 exe（regedit / explorer / mmc）一律用 1033(en-US)，
    # 用 0（中性语言）会被 SHGetFileInfo 拒绝 → 报 ERROR_NO_DATA(1008)、无图标。
    # 故这里要求与系统一致。
    for path, _lf in leaves:
        if len(path) == 3 and path[2] != 1033:
            ok = False
            print(f"  FAIL: lang 应为 1033(en-US，与系统 exe 一致)，实际 {path[2]}")
            break

    # RT_ICON 的 id 必须是连续的 1..n —— GROUP_ICON 的 nID 按序号索引它们
    icon_ids = sorted(p[1] for p, _ in leaves if p and p[0] == 3 and len(p) > 1)
    if icon_ids and icon_ids != list(range(1, len(icon_ids) + 1)):
        ok = False
        print(f"  FAIL: RT_ICON 的 id 应为连续 1..{len(icon_ids)}，实际 {icon_ids}")

    desc = (f"叶子层深分布 {depth_hist}（应为全 3）; "
            f"RT_ICON id={icon_ids[:3]}...{icon_ids[-1:] if icon_ids else []}"
            f" 共 {len(icon_ids)} 个, lang=1033")
    return ok, desc


def check_syso(path):
    d = open(path, "rb").read()
    machine, nsec, _ts, psym, nsym, optsz, _ch = struct.unpack("<HHIIIHH", d[:20])
    print(f"== {path}")
    print(f"COFF: Machine=0x{machine:X} sections={nsec} optSize={optsz} "
          f"symPtr={psym} symCount={nsym}")
    ok = True
    if machine != 0x8664:
        print("  FAIL: Machine 应为 0x8664 (amd64)")
        ok = False
    if nsec != 1:
        print(f"  FAIL: 应恰好 1 个节，实际 {nsec}")
        ok = False

    hdrs = []
    for i in range(nsec):
        o = 20 + i * 40
        name = d[o:o + 8].rstrip(b"\x00").decode("ascii", "replace")
        # 节头字段偏移：Name(0..8) VirtualSize(8..12) VirtualAddress(12..16)
        #            SizeOfRawData(16..20) PointerToRawData(20..24)
        #            PointerToRelocations(24..28) PointerToLinenumbers(28..32)
        #            NumberOfRelocations(32..34) NumberOfLinenumbers(34..36)
        #            Characteristics(36..40)
        vsz, va, rsz, ra = struct.unpack("<IIII", d[o + 8:o + 24])
        prel = struct.unpack("<I", d[o + 24:o + 28])[0]
        nrel = struct.unpack("<H", d[o + 32:o + 34])[0]
        chars = struct.unpack("<I", d[o + 36:o + 40])[0]
        hdrs.append((name, vsz, va, rsz, ra, prel, nrel, chars))
        print(f"  节[{i}] {name!r} VSize={vsz} VA=0x{va:X} RawSize={rsz} "
              f"PtrRaw=0x{ra:X} PtrReloc=0x{prel:X} nReloc={nrel} Char=0x{chars:X}")
        if name != ".rsrc":
            print("  FAIL: 节名必须是 .rsrc（链接器按名识别）")
            ok = False
        if ra != (20 + 40 * (i + 1) + (-(20 + 40 * (i + 1))) % 4):
            print(f"  FAIL: PtrRaw 必须是 4 字节对齐且紧跟节头之后（应为 "
                  f"{20 + 40 * (i + 1)}）")
            ok = False

    if not hdrs:
        return False
    name, vsz, va, rsz, ra, prel, nrel, chars = hdrs[0]
    sec = d[ra:ra + rsz]

    # ── 符号表：必须有一个 Name=".rsrc" 的 STATIC 符号 ──────────────
    if nsym < 1:
        print("  FAIL: 符号表为空 —— 链接器依赖符号名识别 .rsrc")
        ok = False
    else:
        sym = d[psym:psym + 18]
        s_name = sym[0:8].rstrip(b"\x00").decode("ascii", "replace")
        s_val = struct.unpack("<I", sym[4:8])[0]
        s_idx = struct.unpack("<h", sym[8:10])[0]
        s_type = sym[10]
        s_class = sym[11]
        n_aux = struct.unpack("<H", sym[16:18])[0]
        print(f"  符号[0]: name={s_name!r} value={s_val} strIdx={s_idx} "
              f"type={s_type} class={s_class}(3=STATIC) nAux={n_aux}")
        if s_name != ".rsrc" or s_class != 3:
            print("  FAIL: 符号名应为 .rsrc 且 class 应为 3(STATIC)")
            ok = False

    # ── 重定位表：每个 DATA_ENTRY 的 OffsetToData 都要一条 ──────────
    reloc_offs = []
    for r in range(nrel):
        va_, si, ty = struct.unpack("<IIH", d[prel + r * 10:prel + r * 10 + 10])
        reloc_offs.append(va_)
    print(f"  重定位 {nrel} 条: " +
          ", ".join(f"0x{v:X}(T{t})" for v, t in
                    [(struct.unpack("<IIH", d[prel + r * 10:prel + r * 10 + 10])[0],
                      struct.unpack("<IIH", d[prel + r * 10:prel + r * 10 + 10])[2])
                     for r in range(nrel)]))
    if nrel == 0:
        print("  FAIL: 无重定位条目 —— OffsetToData 不会被修正成 RVA")
        ok = False

    # ── 资源树：递归展开，收集所有叶子 DATA_ENTRY ────────────────────
    n, i, ent = _dir_at(sec, 0)
    print(f"  根目录: named={n} id={i}")
    leaves = _walk(sec, 0)
    if not leaves:
        print("  FAIL: 资源树为空")
        return False
    by_type = _group_by_type(leaves)
    print("  资源类型: " + ", ".join(
        f"{RT_NAMES.get(k, k)}({len(v)})" for k, v in sorted(by_type.items())))
    for t in (3, 14, 16):
        if t not in by_type:
            print(f"  FAIL: 缺 {RT_NAMES.get(t, t)}")
            ok = False

    shape_ok, shape_desc = _check_tree_shape(leaves)
    print(f"  树形: {shape_desc}")
    ok = ok and shape_ok

    # 逐个叶子校验：OffsetToData 在界内、对应字段有重定位、内容合法
    data_entries = []
    for t, tname in ((14, "RT_GROUP_ICON"), (16, "RT_VERSION")):
        for lf in by_type.get(t, []):
            drva, dsz, _cp, _rs = struct.unpack("<IIII", sec[lf:lf + 16])
            data_entries.append((tname, lf, drva, dsz))
            reloc_mark = "有重定位" if lf in reloc_offs else "**无重定位**"
            print(f"  {tname}: DATA_ENTRY@0x{lf:X} OffsetToData=0x{drva:X} "
                  f"Size={dsz}  {reloc_mark}")
            if lf not in reloc_offs:
                print(f"  FAIL: {tname} 的 OffsetToData 字段无重定位条目")
                ok = False
            if not (0 < drva < len(sec)):
                print(f"  FAIL: {tname} OffsetToData 越界")
                ok = False
            if dsz == 0:
                print(f"  FAIL: {tname} Size 为 0")
                ok = False

    icon_leaves = by_type.get(3, [])
    print(f"  RT_ICON: {len(icon_leaves)} 个叶子 DATA_ENTRY")
    for idx, lf in enumerate(sorted(icon_leaves), 1):
        drva, dsz, _cp, _rs = struct.unpack("<IIII", sec[lf:lf + 16])
        data_entries.append((f"RT_ICON#{idx}", lf, drva, dsz))
        if lf not in reloc_offs:
            print(f"  FAIL: RT_ICON#{idx} 的 OffsetToData 无重定位条目")
            ok = False
        blob = sec[drva:drva + dsz] if 0 < drva < len(sec) else b""
        if blob[:4] != b"\x89PNG":
            print(f"  FAIL: RT_ICON#{idx} 数据不是 PNG（OffsetToData=0x{drva:X}）")
            ok = False

    # ── GROUP_ICON 内容：列出各尺寸条目 ───────────────────────────
    for label, lf, drva, dsz in data_entries:
        if label != "RT_GROUP_ICON":
            continue
        resv, typ_, cnt = struct.unpack("<HHH", sec[drva:drva + 6])
        print(f"  GRPICONDIR: reserved={resv} type={typ_}(应1) count={cnt}")
        if typ_ != 1 or cnt == 0:
            print("  FAIL: GRPICONDIR 无效")
            ok = False
            continue
        p = drva + 6
        dims = []
        for _ in range(cnt):
            # GRPICONDIRENTRY 紧凑 14 字节（nID 是 WORD）。按 16 字节读会
            # 把 padding 当字段，条目步进也错 → 尺寸列出来是乱的。
            #
            # 注意尾部切片：sec[p:p+16] 可能不足 16 字节（最后一个条目靠节尾），
            # struct.unpack 会直接抛 "requires a buffer of 16 bytes"。
            # 故先按剩余长度裁剪，再 pack 补齐。
            chunk = sec[p:p + 14]
            if len(chunk) < 14:
                print("  FAIL: GRPICONDIR 条目越界")
                ok = False
                break
            # 字段偏移：w,h,bColorCount,bReserved (0..4) | wPlanes,bBitCount (4..8)
            #           | dwBytesInRes (8..12) | nID (12..14)
            w, h, _cc, _rs = struct.unpack("<BBBB", chunk[0:4])
            _pl, bpp_ = struct.unpack("<HH", chunk[4:8])
            by_ = struct.unpack("<I", chunk[8:12])[0]
            nid_ = struct.unpack("<H", chunk[12:14])[0]
            dims.append((w or 256, h or 256, nid_, by_, bpp_))
            p += 14
        print("    " + ", ".join(f"{w}x{h}→id={nid_}({by_}B,{bpp_}bpp)"
                                for w, h, nid_, by_, bpp_ in dims))
        if cnt != len(by_type.get(3, [])):
            print(f"  FAIL: GROUP_ICON 条目数 {cnt} 与 ICON 数据条目数 "
                  f"{len(by_type.get(3, []))} 不一致")
            ok = False

    # ── VERSION 内容 ─────────────────────────────────────────────
    for label, lf, drva, dsz in data_entries:
        if label != "RT_VERSION":
            continue
        u = sec[drva:drva + dsz].decode("utf-16-le", "replace")
        has = "omnigate" in u
        print(f"  RT_VERSION {dsz}B, 含 ProductName: {has}")
        if not has:
            print("  FAIL: VS_VERSIONINFO 缺少 ProductName")
            ok = False

    print("  " + ("PASS" if ok else "FAIL"))
    return ok


def check_exe(path):
    """校验最终 exe：资源数据目录 + 每个 OffsetToData 是否已重定位为 RVA。"""
    d = open(path, "rb").read()
    print(f"== {path}")
    if d[:2] != b"MZ":
        print("  FAIL: 不是 PE")
        return False
    pe = struct.unpack("<I", d[0x3C:0x40])[0]
    if d[pe:pe + 4] != b"PE\x00\x00":
        print("  FAIL: PE 签名错")
        return False
    nsec = struct.unpack("<H", d[pe + 6:pe + 8])[0]
    optsz = struct.unpack("<H", d[pe + 20:pe + 22])[0]
    opt = pe + 24
    magic = struct.unpack("<H", d[opt:opt + 2])[0]
    ddir = opt + (112 if magic == 0x20B else 96)
    rrva, rsz = struct.unpack("<II", d[ddir + 16:ddir + 24])
    print(f"PE32{'+' if magic == 0x20B else ''} sections={nsec} "
          f".rsrc RVA=0x{rrva:X} Size={rsz}")
    if rrva == 0:
        print("  FAIL: PE 数据目录[2] 为 0 —— exe 不含任何资源")
        return False

    secs = []
    sec = opt + optsz
    for i in range(nsec):
        o = sec + i * 40
        # 同上：VirtualSize/VirtualAddress/SizeOfRawData/PointerToRawData
        vsz, va, srsz, ra = struct.unpack("<IIII", d[o + 8:o + 24])
        secs.append((va, vsz, srsz, ra))

    def r2o(r):
        for va, vsz, srsz, ra in secs:
            if va <= r < va + max(vsz, srsz):
                return ra + (r - va)
        return None

    base = r2o(rrva)
    ok = True
    # 资源树内部的所有偏移都是**节内相对**（不是文件绝对），
    # 故先切出节内容再递归，否则第一层就跳到文件别处 → 树为空。
    sec_buf = d[base:base + rsz]
    leaves = _walk(sec_buf, 0)
    if not leaves:
        print("  FAIL: 资源树为空")
        return False
    by_type = _group_by_type(leaves)
    print("  资源类型: " + ", ".join(
        f"{RT_NAMES.get(k, k)}({len(v)})" for k, v in sorted(by_type.items())))
    for t in (3, 14, 16):
        if t not in by_type:
            print(f"  FAIL: 缺 {RT_NAMES.get(t, t)}")
            ok = False

    shape_ok, shape_desc = _check_tree_shape(leaves)
    print(f"  树形: {shape_desc}")
    ok = ok and shape_ok

    # RT_GROUP_ICON
    if 14 in by_type:
        lf = base + by_type[14][0]
        drva, dsz, _cp, _rs = struct.unpack("<IIII", d[lf:lf + 16])
        # 关键判据：OffsetToData 必须是落在 .rsrc 范围内的 **RVA**，
        # 而不是节内小偏移。后者说明重定位没生效。
        in_sec = rrva <= drva < rrva + rsz
        print(f"  GROUP OffsetToData=0x{drva:X} "
              f"({'是 RVA，重定位已生效' if in_sec else '**仍是节内偏移，未重定位**'})")
        if not in_sec:
            ok = False
        g = r2o(drva)
        if g is None:
            print("  FAIL: OffsetToData 指向无效 RVA")
            return False
        resv, typ_, cnt = struct.unpack("<HHH", d[g:g + 6])
        print(f"  GRPICONDIR type={typ_} count={cnt}")
        if typ_ != 1 or cnt == 0:
            print("  FAIL: GRPICONDIR 无效")
            ok = False
        else:
            p = g + 6
            dims = []
            for _ in range(cnt):
                # GRPICONDIRENTRY 紧凑 14 字节；尾部可能不足 16 字节，故按需裁剪
                chunk = d[p:p + 14]
                if len(chunk) < 14:
                    print("  FAIL: GRPICONDIR 条目越界")
                    ok = False
                    break
                w, h, _cc, _rs = struct.unpack("<BBBB", chunk[0:4])
                _pl, _bpp = struct.unpack("<HH", chunk[4:8])
                by = struct.unpack("<I", chunk[8:12])[0]
                nid = struct.unpack("<H", chunk[12:14])[0]
                dims.append((w or 256, h or 256, nid, by))
                p += 14
            print("    " + ", ".join(f"{w}x{h}→id={nid}({by}B)"
                                    for w, h, nid, by in dims))

    # 逐个 ICON 验证 PNG 可达
    good = 0
    il = by_type.get(3, [])
    for idx, lfo in enumerate(sorted(il), 1):
        drva, dsz, _cp, _rs = struct.unpack("<IIII", d[base + lfo:base + lfo + 16])
        if not (rrva <= drva < rrva + rsz):
            print(f"  FAIL: ICON#{idx} OffsetToData=0x{drva:X} 未重定位")
            ok = False
            continue
        o = r2o(drva)
        if o is not None and d[o:o + 4] == b"\x89PNG":
            good += 1
        else:
            print(f"  FAIL: ICON#{idx} 数据不是 PNG")
            ok = False
    print(f"  ICON 数据校验: {good}/{len(il)} 为合法 PNG")
    if good != len(il):
        ok = False

    # RT_VERSION
    if 16 in by_type:
        lf = base + by_type[16][0]
        drva, dsz, _cp, _rs = struct.unpack("<IIII", d[lf:lf + 16])
        o = r2o(drva)
        if o is not None:
            u = d[o:o + dsz].decode("utf-16-le", "replace")
            print(f"  RT_VERSION {dsz}B: ProductName={'omnigate' in u} "
                  f"Description={'账号聚合服务' in u}")

    print("  " + ("PASS" if ok else "FAIL"))
    return ok


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    results = []
    for p in sys.argv[1:]:
        try:
            if p.endswith(".syso"):
                results.append(check_syso(p))
            elif p.endswith(".exe"):
                results.append(check_exe(p))
            else:
                print(f"跳过 {p}")
        except Exception as e:
            print(f"  ERROR {p}: {e}")
            results.append(False)
    sys.exit(0 if all(results) else 1)


if __name__ == "__main__":
    main()