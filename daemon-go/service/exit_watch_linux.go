//go:build linux

package service

import (
	"golang.org/x/sys/unix"
)

// waitExit blocks until pid exits (true) or cancel is closed (false), using a
// pidfd (Linux 5.3+), which works for a non-child pid of the same user. A
// pipe wakes the poll for cancellation. armed runs once the pidfd is open,
// so the caller can re-check the process identity; false (not the process it
// meant) ends the wait as an exit.
func waitExit(pid int, cancel <-chan struct{}, armed func() bool) (exited bool, err error) {
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if err == unix.ESRCH {
			return true, nil // already gone
		}
		return false, err
	}
	defer unix.Close(pidfd)
	var wake [2]int
	if err := unix.Pipe2(wake[:], unix.O_CLOEXEC); err != nil {
		return false, err
	}
	defer unix.Close(wake[0])
	if armed != nil && !armed() {
		return true, nil
	}
	defer unix.Close(wake[1])
	defer stopWaker(cancel, func() { _, _ = unix.Write(wake[1], []byte{1}) })()
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}, {Fd: int32(wake[0]), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return false, err
		}
		if fds[0].Revents != 0 {
			return true, nil
		}
		if fds[1].Revents != 0 {
			return false, nil
		}
	}
}
