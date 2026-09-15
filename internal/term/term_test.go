package term

import (
	"os"
	"runtime"
	"testing"
)

// TestNullIsNotATerminal: /dev/null is a character device but not a terminal —
// a run with stdin redirected from it must not be asked questions.
func TestNullIsNotATerminal(t *testing.T) {
	name := "/dev/null"
	if runtime.GOOS == "windows" {
		name = "NUL"
	}
	f, err := os.Open(name)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Fatalf("%s reported as a terminal", name)
	}
	if Width(f) != 0 {
		t.Fatalf("%s has no width", name)
	}
	if _, err := EchoOff(f); err == nil {
		t.Fatalf("echo control on %s should fail", name)
	}
}
