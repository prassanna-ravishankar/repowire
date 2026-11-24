//go:build darwin

package service

import (
	"golang.org/x/sys/unix"
)

// waitExit blocks until pid exits (true) or cancel is closed (false), using
// kqueue EVFILT_PROC/NOTE_EXIT, which works for a non-child pid of the same
// user. A user event on the same queue wakes it for cancellation. armed runs
// once the watch is registered, so the caller can re-check the process
// identity and not miss an exit between its check and the arming.
func waitExit(pid int, cancel <-chan struct{}, armed func()) (exited bool, err error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return false, err
	}
	defer unix.Close(kq)
	const wake = 1
	changes := []unix.Kevent_t{
		{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT},
		{Ident: wake, Filter: unix.EVFILT_USER, Flags: unix.EV_ADD | unix.EV_CLEAR},
	}
	if _, err := unix.Kevent(kq, changes, nil, nil); err != nil {
		if err == unix.ESRCH {
			return true, nil // already gone
		}
		return false, err
	}
	if armed != nil {
		armed()
	}
	defer stopWaker(cancel, func() {
		_, _ = unix.Kevent(kq, []unix.Kevent_t{{Ident: wake, Filter: unix.EVFILT_USER, Fflags: unix.NOTE_TRIGGER}}, nil, nil)
	})()
	events := make([]unix.Kevent_t, 2)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, ev := range events[:n] {
			if ev.Filter == unix.EVFILT_PROC {
				return true, nil
			}
			if ev.Filter == unix.EVFILT_USER {
				return false, nil
			}
		}
	}
}
