# Windows 图标与 exe 资源

`dist/omnigate.exe` 带图标和版本信息（资源管理器属性里能看到产品名）。
这些资源来自 `cmd/omnigate/rsrc_windows_amd64.syso`，由本目录两个脚本从
`windows/icon.ico` 生成。

## 用法

```bash
# 1. 改图标：编辑 make_icon.py 顶部的配色 / W_PATH，然后
python build/make_icon.py          # → build/windows/icon.ico + icon_preview.png

# 2. 生成 exe 资源
python build/make_syso.py          # → cmd/omnigate/rsrc_windows_amd64.syso

# 3. 构建 + 校验（校验不过就别发布）
bash scripts/build_release.sh          # 发布用构建（含 -trimpath -s -w + 产物自检）
python build/check_res.py cmd/omnigate/rsrc_windows_amd64.syso dist/omnigate.exe
```

改了图标后还要同步托盘与弹层 logo：

```bash
python build/make_icon.py          # 顺带刷新 build/logo64.png
cp build/windows/icon.ico cmd/omnigate/build/trayicon.ico
cp build/logo64.png cmd/omnigate/web/logo64.png
```

## 为什么校验是必须的

嵌入图标这条链路上，**任何一步出错都不会让构建失败**：

| 出错点 | 现象 |
|---|---|
| syso 节名不是 `.rsrc` | 构建成功，exe 无图标 |
| `PointerToRawData` 算错 | 构建成功，exe 无图标 |
| 缺符号表 / 重定位表 | 构建成功，exe 无图标（资源目录指针全错） |
| `OffsetToData` 未重定位成 RVA | 构建成功，exe 无图标 |
| `GRPICONDIRENTRY` 里 `nID` 用了 DWORD | 图标异常或不显示 |

`go build` 全程无报错，exe 也能正常启动——只有肉眼在资源管理器里看得到。
所以 `check_res.py` 是**唯一的判据**，构建后必须跑。

`check_res.py` 的关键判据：exe 里每个 `IMAGE_RESOURCE_DATA_ENTRY.OffsetToData`
都必须是落在 `.rsrc` 节范围内的 **RVA**，而不是节内小偏移。后者说明重定位没生效。

## 为什么不用 rsrc / windres

- `akavel/rsrc` 需要 `go install`，本机 GOPROXY 不稳；
- `windres`（binutils）没装。

手写生成器的额外好处：注释里能讲清 PE 资源布局，这正是排查时最需要的知识。

## 资源树必须是三层（且 lang 用 1033）

```
根目录 → type(RT_ICON/RT_GROUP_ICON/RT_VERSION) → id → lang → DATA_ENTRY
```

两个方向的错都表现为「构建成功、能启动、资源管理器不显示图标」：

| 错误 | 现象 |
|---|---|
| type 与 id 之间**多了**一层 | `FindResource(RT_ICON, id>1)` 找不到；GROUP_ICON 的 nID 全部失效 |
| **少了** lang 层 | Windows 按语言 id 定位资源，缺失时 `SHGetFileInfoW` 报 `ERROR_NO_DATA(1008)` |
| lang 写 **0**（以为「中性语言」） | 同样报 1008 —— 实测系统 exe（regedit / explorer / mmc）一律用 **1033** |
| 多个 id **共用**一个 lang 目录 | 加载器把整个目录读作每个 id 的子目录 → 每个 id 遍历出 n 个叶子 |

每个 id 需要**独立的 lang 目录**（各含 1 个 lang 条目）：资源目录项的
`OffsetToData` 指向子目录起点，其后连续条目都属该子目录。

### RT_ICON 存的是裸 PNG

不是 .ico 文件格式（无 ICONDIR 头）。实测 regedit 的 RT_ICON 同样是裸 PNG，
可直接放 PNG 数据而无需构造 BMP DIB + 调色板 + AND 掩码。

## 用 Windows API 验证（比自己的解析器可靠）

`check_res.py` 查的是「自己写的资源树是否符合规范」，而最终判据是
「Windows 认不认」。用 ctypes 走真实加载路径：

```python
k32.LoadLibraryExW(exe, None, 0x2 | 0x20)      # AS_DATAFILE | AS_IMAGE_RESOURCE
h  = k32.FindResourceExW(hmod, MAKEINTRESOURCE(14),
                         MAKEINTRESOURCE(1), MAKEINTRESOURCE(1033))
sz = k32.SizeofResource(hmod, h)
d  = ctypes.string_at(k32.LockResource(k32.LoadResource(hmod, h)), sz)
```

**注意**：`SHGetFileInfoW` 在 ctypes 里调用不可靠 —— 实测对**系统自带的
regedit.exe 也返回 1008**，所以不能拿它当判据（会误判成「我的 exe 坏了」）。
`FindResourceExW` + `LoadResource` 才是有效路径。

排查时的对照物很有用：拿 `C://Windows//System32//regedit.exe` 解出资源树，
和自己的逐字段对比，比读文档快得多（lang=1033 就是这么发现的）。

## syso 结构速查

COFF 文件（Go 链接器扫描主包目录下的 `*.syso`，当作目标文件链接进去）：

```
[0]   COFF 头 20B  Machine=0x8664(amd64) sections=1
[20]  节头 40B     Name=".rsrc"  ← 链接器按名字识别，写错会静默忽略
[60]  .rsrc 节数据（节内偏移 = 资源树内部偏移）
[..] 重定位表      每个 OffsetToData DWORD 一条 Type=3(IMAGE_REL_AMD64_ADDR32NB)
[..] 符号表 18B    Name=".rsrc"(内联前 8B) Class=3(STATIC)
[..] 字符串表 4B
```

`.rsrc` 节内是标准 `IMAGE_RESOURCE_DIRECTORY`，**三层目录**：

```
根目录 → type(RT_ICON / RT_GROUP_ICON / RT_VERSION) → id → 叶子 DATA_ENTRY
```

两个易错点：

- **`OffsetToData` 必须是「节内偏移」并配一条重定位**。链接器
  （`cmd/link/internal/ld/pe.go` 的 `addpersrc`）用重定位把它改写成
  `virtualAddress + addend`（addend 从文件里那个 DWORD 读）。
  没有重定位 → 偏移保持节内相对值 → 资源指针全错。
- **`GRPICONDIRENTRY` 是 14 字节**，`nID` 是 **WORD(2B)** 不是 DWORD。
  写 4 字节每条目多 2 字节，整块偏移错位。

权威对照样本：`$GOROOT/src/cmd/link/testdata/pe-binutils/rsrc_amd64.syso`。