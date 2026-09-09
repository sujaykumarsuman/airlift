package airlift

import "testing"

func TestDistIsReadable(t *testing.T) {
	entries, err := Dist.ReadDir("web/dist")
	if err != nil {
		t.Fatalf("ReadDir(web/dist): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("web/dist is empty; expected at least .gitkeep")
	}
}
