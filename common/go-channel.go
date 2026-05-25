package common

import (
	"fmt"
	"one-api/common/logger"
	"runtime/debug"
)

func SafeGoroutine(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.SysError(fmt.Sprintf("child goroutine panic occurred: error: %v, stack: %s", r, string(debug.Stack())))
			}
		}()
		f()
	}()
}

func SafeSend(ch chan bool, value bool) (closed bool) {
	defer func() {
		// Recover from panic if one occurred. A panic would mean the channel was closed.
		if recover() != nil {
			closed = true
		}
	}()

	// This will panic if the channel is closed.
	ch <- value

	// If the code reaches here, then the channel was not closed.
	return false
}
