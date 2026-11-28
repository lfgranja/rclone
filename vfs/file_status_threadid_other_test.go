//go:build !linux

package vfs

// fileStatusThreadID returns 0 where per-thread Linux schedstat is unavailable.
func fileStatusThreadID() int { return 0 }
