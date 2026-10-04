//go:build !race

package gateway

import "time"

const wsCloseSchedulingTolerance = 200 * time.Millisecond
