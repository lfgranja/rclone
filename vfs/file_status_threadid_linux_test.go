//go:build linux

package vfs

import "syscall"

// fileStatusThreadID returns the OS thread ID of the calling goroutine on Linux.
func fileStatusThreadID() int { return syscall.Gettid() }
