//go:build !darwin && !linux

package testkit

import "os"

// 其它平台只有预检与 descriptor 复核，不声称能抵抗预检后的恶意文件系统替换。
func openFixtureDescriptor(path string) (*os.File, error) {
	return os.Open(path)
}
