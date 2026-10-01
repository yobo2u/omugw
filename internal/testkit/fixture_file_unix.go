//go:build darwin || linux

package testkit

import (
	"os"
	"syscall"
)

// 非阻塞且不跟随末级链接，防止 Lstat 后被换成 FIFO 时 Open 无限等写端。
func openFixtureDescriptor(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
