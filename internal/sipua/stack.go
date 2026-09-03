package sipua

import (
	"runtime"
)

// stack renders the current goroutine's stack for panic logging (NFR-9).
func stack() string {
	buf := make([]byte, 8192)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}
