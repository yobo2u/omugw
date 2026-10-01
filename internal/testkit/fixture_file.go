package testkit

import (
	"fmt"
	"os"
)

// 打开前拒绝特殊文件，打开后检查同一普通文件，防止 FIFO 等待和末级路径替换绕过预算。
func openFixtureRegular(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("fixture 路径检查失败")
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("fixture 必须为普通文件且不能为符号链接")
	}
	return openFixtureAfterCheck(path, before)
}

func openFixtureAfterCheck(path string, before os.FileInfo) (*os.File, error) {
	file, err := openFixtureDescriptor(path)
	if err != nil {
		return nil, fmt.Errorf("fixture 文件安全打开失败")
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, fmt.Errorf("fixture 普通文件类型或身份复核失败")
	}
	return file, nil
}
