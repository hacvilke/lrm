package lca

import (
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/vectorclock"
)

func mkCommit(t *testing.T, d *dag.Store, parents []cas.Hash, ts int64) cas.Hash {
	t.Helper()
	var ps []string
	for _, p := range parents {
		ps = append(ps, cas.Hex(p))
	}
	c := &dag.Commit{Version: 1, Tree: "t", Parents: ps, Author: "u", PeerHex: "p", Timestamp: ts, Message: "m", Clock: vectorclock.Clock{}}
	h, err := d.Put(c)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestFindLCA(t *testing.T) {
	casStore, _ := cas.New(t.TempDir())
	d := dag.New(casStore)
	root := mkCommit(t, d, nil, 1)
	a1 := mkCommit(t, d, []cas.Hash{root}, 2)
	b1 := mkCommit(t, d, []cas.Hash{root}, 3)
	a2 := mkCommit(t, d, []cas.Hash{a1}, 4)

	got, err := Find(d, a2, b1)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatal("LCA should be root")
	}
	got, _ = Find(d, a1, a2)
	if got != a1 {
		t.Fatal("LCA(a1,a2) should be a1")
	}
	anc, _ := IsAncestor(d, root, a2)
	if !anc {
		t.Fatal("root should be ancestor of a2")
	}
	div, err := Diverged(d, root, a2)
	if err != nil || len(div) != 2 {
		t.Fatalf("diverged=%v err=%v", div, err)
	}
}
