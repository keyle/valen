package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestHighlightDiffRows(t *testing.T) {
	rows := highlightDiffRows("a.go", []string{"+ func main() {", "+ \treturn", "… +3 more"}, 40)
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	for i, want := range []string{"+ func main() {", "+     return", "… +3 more"} {
		if got := strings.TrimSpace(ansi.Strip(rows[i])); got != want {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
	}
	long := highlightDiffRows("x.unknownext", []string{"- " + strings.Repeat("a", 100)}, 20)
	if w := ansi.StringWidth(long[0]); w > 20 {
		t.Errorf("width = %d, want <= 20", w)
	}
}
