// rate.go: 毫秒向上取整 (低性能锁 / 观测 / 响应时长共用)。低性能锁的样本与判定见 gate_lowperf.go (每请求性能值 r* 口径)。

package main

import (
	"time"
)

// msCeil 把时长向上取整为毫秒 (>0 才计, 不计负值或 0)。
func msCeil(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}
