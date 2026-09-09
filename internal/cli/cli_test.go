package cli

import (
	"reflect"
	"testing"
)

// Regression test: flagVal must return the value even when more flags follow
// (an append-aliasing bug once made `init --user alice --port 18443` record
// user "18443").
func TestFlagValMultiple(t *testing.T) {
	args := []string{"--user", "alice", "--port", "18443"}
	user, rest := flagVal(args, "--user", "-u")
	if user != "alice" {
		t.Fatalf("user=%q want alice", user)
	}
	if !reflect.DeepEqual(rest, []string{"--port", "18443"}) {
		t.Fatalf("rest=%q", rest)
	}
	port, rest := flagVal(rest, "--port", "-p")
	if port != "18443" {
		t.Fatalf("port=%q", port)
	}
	if len(rest) != 0 {
		t.Fatalf("rest=%q want empty", rest)
	}
	// Original slice must be unmutated.
	if !reflect.DeepEqual(args, []string{"--user", "alice", "--port", "18443"}) {
		t.Fatalf("input mutated: %q", args)
	}
}

func TestFlagValEqualsForm(t *testing.T) {
	v, rest := flagVal([]string{"--port=9999", "x"}, "--port")
	if v != "9999" || !reflect.DeepEqual(rest, []string{"x"}) {
		t.Fatalf("v=%q rest=%q", v, rest)
	}
}

func TestHasFlag(t *testing.T) {
	ok, rest := hasFlag([]string{"a", "--init", "b"}, "--init")
	if !ok || !reflect.DeepEqual(rest, []string{"a", "b"}) {
		t.Fatalf("ok=%v rest=%q", ok, rest)
	}
}
