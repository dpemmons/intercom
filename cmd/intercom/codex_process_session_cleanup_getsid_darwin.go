//go:build darwin

package main

import "syscall"

func codexProcessGetsid(pid int) (int, error) {
	return syscall.Getsid(pid)
}
