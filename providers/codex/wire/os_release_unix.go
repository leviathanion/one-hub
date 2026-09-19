//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package wire

import "golang.org/x/sys/unix"

func osRelease() string {
	var info unix.Utsname
	if err := unix.Uname(&info); err != nil {
		return "unknown"
	}
	release := make([]byte, 0, len(info.Release))
	for _, c := range info.Release {
		if c == 0 {
			break
		}
		release = append(release, byte(c))
	}
	return string(release)
}
