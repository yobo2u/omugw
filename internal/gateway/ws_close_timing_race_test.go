//go:build race

package gateway

import "time"

const wsCloseSchedulingTolerance = 500 * time.Millisecond
