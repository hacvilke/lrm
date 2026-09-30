package merkle

import (
	"fmt"
	"strings"
)

// CleanTreePath validates a slash-separated path as it may appear in a tree
// object or a flattened tree map, returning the path unchanged when it is
// safe to use as a relative filesystem path.
//
// Tree objects arrive from peers, so a hostile tree must never be able to
// name a location outside the working directory. Rejected:
//
//	""              empty
//	"/etc/passwd"   absolute
//	"a/../b"        traversal segments
//	"a//b"          empty segments (ambiguous under join)
//	"a\x00b"        NUL (truncates syscall strings)
//	`a\b`           backslash (a separator on Windows, a name on Unix)
//
// The path is returned with backslashes rejected outright rather than
// translated: a tree written on one platform must mean one thing on every
// platform, and silently reinterpreting separators is how escapes happen.
func CleanTreePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.ContainsAny(p, "\x00") {
		return "", fmt.Errorf("path %q contains NUL", p)
	}
	// Control characters and Unicode bidi overrides are not filesystem
	// escapes, but they are display spoofs: a hostile tree can name a file
	// so that a listing or GUI shows something other than what it is
	// (U+202E reverses the rendered name). Nothing legitimate needs them.
	for _, r := range p {
		if r < 0x20 || r == 0x7f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return "", fmt.Errorf("path %q contains control or bidi character U+%04X", p, r)
		}
	}
	if strings.Contains(p, "\\") {
		return "", fmt.Errorf("path %q contains backslash", p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return "", fmt.Errorf("path %q has an empty segment", p)
		case ".":
			return "", fmt.Errorf("path %q has a current-directory segment", p)
		case "..":
			return "", fmt.Errorf("path %q escapes the repository", p)
		}
	}
	return p, nil
}

// CleanTreePaths validates every path in a flattened tree before anything
// is written. It returns the first offender, so callers can refuse the
// whole operation: a tree with even one hostile entry is not a tree we
// half-apply.
func CleanTreePaths(paths []string) error {
	for _, p := range paths {
		if _, err := CleanTreePath(p); err != nil {
			return err
		}
	}
	return nil
}
