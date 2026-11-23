//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

type containerChildIsolation struct{}

func (containerChildIsolation) Close()                         {}
func (containerChildIsolation) Err() error                     { return nil }
func isolateChild(*os.Process, uint64) (childIsolation, error) { return containerChildIsolation{}, nil }

func prepareChildCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
