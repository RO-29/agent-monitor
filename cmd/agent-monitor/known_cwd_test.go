package main

import "testing"

// allowCwds makes dirs known folders for the length of the test.
func allowCwds(t *testing.T, dirs ...string) {
	t.Helper()
	knownCwdExtra.mu.Lock()
	old := knownCwdExtra.dirs
	knownCwdExtra.dirs = append(append([]string{}, old...), dirs...)
	knownCwdExtra.mu.Unlock()
	t.Cleanup(func() {
		knownCwdExtra.mu.Lock()
		knownCwdExtra.dirs = old
		knownCwdExtra.mu.Unlock()
	})
}
