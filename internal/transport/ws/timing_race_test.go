//go:build race

package ws

import "time"

// race 的固定容差只用于验收上界，绝对期限本身仍须与普通构建相同。
const closeDeadlineSchedulingTolerance = 500 * time.Millisecond
