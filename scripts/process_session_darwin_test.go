//go:build darwin

package scripts

import "syscall"

func testGetSID(pid int) (int, error) {
	return syscall.Getsid(pid)
}
