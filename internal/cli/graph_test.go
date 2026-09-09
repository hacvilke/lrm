package cli

import (
	"strings"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
)

func fakeHash(b byte) cas.Hash {
	var h cas.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func TestRenderGraphLinear(t *testing.T) {
	c := fakeHash(3)
	b := fakeHash(2)
	a := fakeHash(1)
	lookup := map[cas.Hash]*dag.Commit{
		a: {Message: "first"},
		b: {Message: "second", Parents: []string{cas.Hex(a)}},
		c: {Message: "third", Parents: []string{cas.Hex(b)}},
	}
	rows := RenderGraph([]cas.Hash{c, b, a}, lookup)
	if len(rows) != 3 {
		t.Fatalf("rows=%v", rows)
	}
	for i, want := range []string{"third", "second", "first"} {
		if !strings.Contains(rows[i], want) || !strings.HasPrefix(rows[i], "* ") {
			t.Fatalf("row %d: %q", i, rows[i])
		}
	}
}

func TestRenderGraphMerge(t *testing.T) {
	root := fakeHash(1)
	la := fakeHash(2) // left branch tip
	rb := fakeHash(3) // right branch tip
	m := fakeHash(4)  // merge
	lookup := map[cas.Hash]*dag.Commit{
		root: {Message: "root"},
		la:   {Message: "left", Parents: []string{cas.Hex(root)}},
		rb:   {Message: "right", Parents: []string{cas.Hex(root)}},
		m:    {Message: "merge it", Parents: []string{cas.Hex(la), cas.Hex(rb)}},
	}
	// Topological order (children first).
	rows := RenderGraph([]cas.Hash{m, rb, la, root}, lookup)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "merge it") || !strings.Contains(joined, "Merge:") {
		t.Fatalf("merge not rendered:\n%s", joined)
	}
	// The two branch tips must appear in different columns (one row has "| *").
	foundFork := false
	for _, r := range rows {
		if strings.HasPrefix(r, "| * ") {
			foundFork = true
		}
	}
	if !foundFork {
		t.Fatalf("no fork column rendered:\n%s", joined)
	}
}
