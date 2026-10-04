//go:build !race

package dashscopeinference

import "time"

// 固定调度容差不能随测试运行时长放宽，否则会放过预算被延长的回归。
const networkSchedulingTolerance = 200 * time.Millisecond
