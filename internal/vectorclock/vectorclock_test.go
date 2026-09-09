package vectorclock

import "testing"

func TestOrdering(t *testing.T) {
	a := Clock{"alice": 2, "bob": 1}
	b := Clock{"alice": 2, "bob": 2}
	if a.Compare(b) != -1 {
		t.Fatal("a should happen-before b")
	}
	if !a.HappenedBefore(b) {
		t.Fatal("HappenedBefore failed")
	}
	if b.Compare(a) != 1 {
		t.Fatal("b should be after a")
	}
}

func TestConcurrent(t *testing.T) {
	a := Clock{"alice": 2}
	b := Clock{"bob": 1}
	if !a.Concurrent(b) {
		t.Fatal("should be concurrent")
	}
	m := a.Clone()
	m.Merge(b)
	if m["alice"] != 2 || m["bob"] != 1 {
		t.Fatal("merge failed")
	}
}
