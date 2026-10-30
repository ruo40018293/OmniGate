// diag.go 内容拦截诊断（R82，吸收上游 wild-work R134）：ErrContentBlocked 时把
// 发往上游的 prepared body 落盘，供 scripts/bisect-blocked.py 做分层重放与二分定位。
//
// 为什么需要它：内容拦截（400 code=11-128 / blocked by security policy）无法用探针
// 复现——同样的请求重发就可能 200。只有真实失败那一刻的「出站字节」能用来定位是哪
// 句话触发的。internal/capture 存的是「请求+响应」快照（还会被体积上限裁剪），
// 这里存的是**纯出站 body 原文**，可直接喂给重放脚本。
//
// 零 token 口径：body 是发往上游的请求体，不含 Authorization 等凭据头；
// [diag] 行只在拦截时产生，正常请求零 IO、零日志。
package upstream

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// diagDir 落盘目录（相对程序工作目录；main.go 启动时 Chdir 到 exe 所在目录，
// 故即部署目录下的 data/blocked/，与 data/app.log 同级）。
const diagDir = "data/blocked"

// diagKeep 滚动保留份数。只留最近 3 份足够定位，且不会把磁盘吃满。
const diagKeep = 3

// diagMaxBytes 目录总字节上限（对齐 R75：取证目录必须「文件数 + 总字节」双上限）。
// 单份 body 可达数 MB（长 system + 多模态 base64），只封份数挡不住目录膨胀。
const diagMaxBytes = 2 << 20 // 2MB

// DumpBlocked 把被拦请求体写入 data/blocked/blocked-<unixnano>.json，
// 滚动保留最近 diagKeep 份。落盘失败只记日志，绝不影响请求返回。
func DumpBlocked(body []byte) {
	if len(body) == 0 {
		return
	}
	if err := os.MkdirAll(diagDir, 0o755); err != nil {
		log.Printf("[diag] 创建 %s 失败: %v", diagDir, err)
		return
	}
	p := filepath.Join(diagDir, fmt.Sprintf("blocked-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(p, body, 0o644); err != nil {
		log.Printf("[diag] 落盘失败: %v", err)
		return
	}
	log.Printf("[diag] 已被拦的请求体已保存: %s（%d 字节）", p, len(body))
	pruneBlocked(diagDir)
}

// pruneBlocked 滚动清理：文件名含 unixnano 时间戳，字典序即时间序，
// 只保留最新 diagKeep 份。
func pruneBlocked(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, "blocked-*.json"))
	if err != nil {
		return
	}
	sort.Strings(matches) // 文件名含 unixnano，字典序即时间序

	// 第一轮：按份数修剪，保留最新 diagKeep 份
	kept := matches
	if len(matches) > diagKeep {
		for _, gone := range matches[:len(matches)-diagKeep] {
			os.Remove(gone)
		}
		kept = matches[len(matches)-diagKeep:]
	}

	// 第二轮（R75）：按总字节修剪。即便不超 diagKeep 份，
	// 多模态 base64 仍可能把目录顶到数百 MB，只封份数是挡不住的。
	var total int64
	sizes := make([]int64, len(kept))
	for i, f := range kept {
		if fi, statErr := os.Stat(f); statErr == nil {
			sizes[i] = fi.Size()
			total += sizes[i]
		}
	}
	for i := 0; i < len(kept) && total > diagMaxBytes; i++ {
		if rmErr := os.Remove(kept[i]); rmErr != nil {
			continue
		}
		total -= sizes[i]
	}
}
