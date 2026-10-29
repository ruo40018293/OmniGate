package app

import (
	"bytes"
	"io"
	"os"
	"strings"
)

// tailLines 取文件最后 n 行；只读尾部一段，不把整个文件读进内存。
//
// 为什么需要它（R73 实测）：`/api/logs` 原来是 os.ReadFile + strings.Split
// 全量读，而 data/app.log 已到 12 MB、面板每 15 秒轮一次 —— 单日约 69 GB 的
// 无谓读盘与字符串分配，只为展示末尾 300 行。改成从尾部读 256 KB 后单次成本
// 与文件大小无关。
//
// 边界处理：
//   - 文件比窗口小：整文件读取；
//   - 从中间开始读时首行可能是半行：丢到第一个换行符之后；
//   - 窗口内没有任何换行（单行超过 256 KB 或整块半行）：返回 nil 而不是返回
//     半行 —— 宁可这轮空着，也不给前端贴一行被腰斩的日志。
//
// 日志行量级是百字节，不为「单行超窗口」再增加复杂度。
func tailLines(fp string, n int) []string {
	if n <= 0 {
		return nil
	}
	f, err := os.Open(fp)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= 0 {
		return nil
	}
	const window = 256 << 10
	off := st.Size() - window
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	buf := make([]byte, st.Size()-off)
	got, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil
	}
	buf = buf[:got]
	if off > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return nil
		}
		buf = buf[i+1:] // 丢掉半行
	}
	text := strings.TrimRight(string(buf), "\r\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimSuffix(l, "\r") // CRLF 日志
	}
	return out
}
