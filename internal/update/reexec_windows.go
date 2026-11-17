package update

import (
	"os"
	"os/exec"
)

// Reexec 启动新的进程（沿用原来的命令行参数、环境变量和控制台），然后退出当前进程。
// Windows 没有 exec，进程号会变化；新进程继承同一个控制台，Ctrl+C 仍然有效。成功时不返回。
func Reexec(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
