package service

import "sync"

// stopWaker runs wake once cancel closes, until the returned stop is called.
// stop returns only once wake can no longer run, so the caller may close the
// descriptors wake uses (a closed descriptor's number can be reused at once).
func stopWaker(cancel <-chan struct{}, wake func()) (stop func()) {
	var mu sync.Mutex
	stopped := false
	done := make(chan struct{})
	go func() {
		select {
		case <-cancel:
			mu.Lock()
			if !stopped {
				wake()
			}
			mu.Unlock()
		case <-done:
		}
	}()
	return func() {
		mu.Lock()
		stopped = true
		mu.Unlock()
		close(done)
	}
}
