package term

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const enableEchoInput = 0x4

func consoleMode(f *os.File) (uint32, bool) {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return mode, r != 0
}

func isTerminal(f *os.File) bool {
	_, ok := consoleMode(f)
	return ok
}

func width(f *os.File) int {
	var info struct {
		Size, CursorPosition     [2]int16
		Attributes               uint16
		Left, Top, Right, Bottom int16
		MaximumWindowSize        [2]int16
	}
	if r, _, _ := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0
	}
	return int(info.Right-info.Left) + 1
}

// echoOff clears ENABLE_ECHO_INPUT on f's console and returns the restorer.
func echoOff(f *os.File) (func(), error) {
	mode, ok := consoleMode(f)
	if !ok {
		return nil, syscall.EINVAL
	}
	if r, _, e := procSetConsoleMode.Call(f.Fd(), uintptr(mode&^enableEchoInput)); r == 0 {
		return nil, e
	}
	return func() { procSetConsoleMode.Call(f.Fd(), uintptr(mode)) }, nil
}
