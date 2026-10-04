//go:build !race

package ws

import "time"

// 固定调度容差不能变成生产网络预算或用更早的测试清理冒充到期退出。
const closeDeadlineSchedulingTolerance = 200 * time.Millisecond
