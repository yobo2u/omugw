//go:build race

package dashscopeinference

import "time"

// race 的额外调度成本使用独立固定容差，不能侵入生产超时配置。
const networkSchedulingTolerance = 500 * time.Millisecond
