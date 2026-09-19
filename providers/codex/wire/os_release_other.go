//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package wire

func osRelease() string { return "unknown" }
