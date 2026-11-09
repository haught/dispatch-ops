//go:build !linux

package runner

// Harden does nothing outside Linux, where job pods don't run.
func Harden() error { return nil }
