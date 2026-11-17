//go:build !windows

package update

import (
	"os"
	"syscall"
)

// Reexec 用 exe 替换当前进程，沿用原来的命令行参数和环境变量。
// 进程号不变，systemd、Docker 等进程管理不会察觉到重启。成功时不返回。
func Reexec(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
