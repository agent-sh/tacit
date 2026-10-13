package derive

import (
	"fmt"
	"strings"
	"testing"
)

// A description holds whole items only and stays within the lint limit.
func TestFitListKeepsWholeItems(t *testing.T) {
	var items []string
	for i := 0; i < 30; i++ {
		items = append(items, fmt.Sprintf("crates/memra-engine/src/module_%02d.rs %d", i, 30-i))
	}
	d := fitList("Hot spots in memra (path, commits): ", items)
	if len([]rune(d)) > 200 {
		t.Fatalf("description is %d characters", len([]rune(d)))
	}
	last := d[strings.LastIndex(d, ", ")+2:]
	if !contains(items, last) {
		t.Fatalf("cut inside an item: %q", last)
	}
	if strings.Count(d, "module_") < 2 {
		t.Fatalf("too few items: %q", d)
	}
	if got := fitList("p: ", []string{"a 1", "b 2"}); got != "p: a 1, b 2" {
		t.Fatalf("short list %q", got)
	}
}
