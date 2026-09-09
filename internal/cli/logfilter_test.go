package cli

import (
	"strings"
	"testing"
)

func TestMatchLogFilter(t *testing.T) {
	cases := []struct {
		msg, author, grep, auth string
		want                    bool
	}{
		{"fix crash", "amy", "", "", true},
		{"fix crash", "amy", "crash", "", true},
		{"fix crash", "amy", "CRASH", "", true}, // needles pre-lowered by caller
		{"fix crash", "amy", "oops", "", false},
		{"fix crash", "amy", "", "amy", true},
		{"fix crash", "amy", "", "bob", false},
		{"fix crash", "amy", "fix", "amy", true},
		{"fix crash", "amy", "fix", "bob", false},
		{"(missing)", "", "fix", "", false},
	}
	for _, c := range cases {
		got := matchLogFilter(c.msg, c.author, strings.ToLower(c.grep), strings.ToLower(c.auth))
		if got != c.want {
			t.Errorf("msg=%q author=%q grep=%q auth=%q: got %v want %v",
				c.msg, c.author, c.grep, c.auth, got, c.want)
		}
	}
}
