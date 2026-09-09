package session

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"unicode"
)

// Two short lowercase wordlists; a generated client name is "<adjective> <noun>"
// (e.g. "calm otter"), friendly and easy to say aloud across the room.
var (
	nameAdjectives = []string{
		"amber", "brave", "calm", "clever", "cosy", "eager", "fond", "gentle",
		"jolly", "keen", "lively", "lucky", "merry", "mild", "neat", "nimble",
		"plucky", "quiet", "ready", "sunny", "swift", "tidy", "warm", "witty",
	}
	nameNouns = []string{
		"otter", "finch", "heron", "lynx", "marten", "newt", "osprey", "puffin",
		"quail", "raven", "robin", "shrew", "sparrow", "stoat", "swift", "teal",
		"vole", "wren", "badger", "beaver", "dolphin", "hare", "ibex", "kestrel",
	}
)

func generateName() string {
	return nameAdjectives[randIndex(len(nameAdjectives))] + " " + nameNouns[randIndex(len(nameNouns))]
}

// randIndex returns a uniform-enough index in [0,n) from the CSPRNG.
func randIndex(n int) int {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return int(binary.BigEndian.Uint64(b[:]) % uint64(n))
}

// cleanName trims a proposed name, drops control characters and caps its length;
// it returns "" when nothing usable remains (the caller then generates one).
func cleanName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 40 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
