package app

// R70：运行日志轮转（data/app.log）。
//
// 为什么需要：app.log 是唯一日志文件且从不轮转，实测 2026-10-29 已 11.4 MB
// 单文件。日志是排障的唯一线索，无界增长既难翻也没法归档。
//
// 为什么不能简单按体积轮转：`internal/stats` 启动时会**全量重扫 app.log**
// 重建当天的签到事件（events.go / stats_feed.go 的双通道之一）。若在启动时
// 把含今日行的 app.log 改名成 app.log.1，重扫就扫不到今天已发生的签到 →
// 面板「今日收入」变 0，直到下一次签到才恢复。
//
// 因此轮转条件是「体积超限 **且** 最后一次写入早于今天 0 点」：
// 典型形态就是第二天首次启动时把昨天那个大文件归档，当天永不搬动。

import (
	"fmt"
	"os"
	"time"
)

const (
	// logRotateLimit 单个日志文件体积上限（20 MB）。日常 ~150 行/天、
	// 单行几百字节，正常一天不到 1 MB，触到上限基本都是异常刷屏。
	logRotateLimit = 20 << 20
	// logRotateSuffix 归档后缀（只保留 1 份，旧的直接覆盖）。
	logRotateSuffix = ".1"
)

// rotateLogIfStale 体积超限且不属于今天时，把 path 归档为 path+".1"。
// 返回是否真的轮转了（便于日志里留痕与测试断言）。
func rotateLogIfStale(path string, limit int64, now time.Time) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Size() < limit {
		return false, nil
	}
	y, m, d := now.Date()
	dayStart := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	if !st.ModTime().Before(dayStart) {
		// 今天写过：里面可能有今天的签到事件，重扫要用，不能搬。
		return false, nil
	}
	dst := path + logRotateSuffix
	_ = os.Remove(dst) // 只留一份归档；删除失败由 Rename 报错兜底
	if err := os.Rename(path, dst); err != nil {
		return false, fmt.Errorf("归档 %s: %w", path, err)
	}
	return true, nil
}
