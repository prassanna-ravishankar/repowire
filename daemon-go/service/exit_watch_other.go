//go:build !darwin && !linux

package service

import "errors"

// waitExit has no implementation on this platform: no watch is armed.
func waitExit(int, <-chan struct{}, func()) (bool, error) {
	return false, errors.New("exit watch unsupported on this platform")
}
