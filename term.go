package main

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

const defaultTermWidth = 80

// termWidth reports the column count of the terminal behind w, falling back
// to $COLUMNS and then to defaultTermWidth.
func termWidth(w any) int {
	if f, ok := w.(*os.File); ok {
		var ws struct{ Row, Col, X, Y uint16 }
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
		if errno == 0 && ws.Col > 0 {
			return int(ws.Col)
		}
	}
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return defaultTermWidth
}
