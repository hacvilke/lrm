// Package vectorclock implements vector clocks for tracking concurrent
// edits across peers (Kafka/Raft-inspired distributed log model).
package vectorclock

import (
	"sort"
	"strings"
)

// Clock maps hex-PeerID → logical counter.
type Clock map[string]uint64

// Clone returns a deep copy.
func (c Clock) Clone() Clock {
	out := make(Clock, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// Increment bumps this peer's counter.
func (c Clock) Increment(peerHex string) {
	c[peerHex] = c[peerHex] + 1
}

// Merge takes the element-wise max with other (mutating receiver).
func (c Clock) Merge(other Clock) {
	for k, v := range other {
		if v > c[k] {
			c[k] = v
		}
	}
}

// Compare returns:
//
//	-1 if c < other (c happened-before other)
//	 1 if c > other (other happened-before c)
//	 0 if equal or concurrent — use Concurrent() to distinguish.
func (c Clock) Compare(other Clock) int {
	cLess, cGreater := false, false
	for k, v := range c {
		ov := other[k]
		if v < ov {
			cLess = true
		} else if v > ov {
			cGreater = true
		}
	}
	for k, v := range other {
		if _, ok := c[k]; !ok && v > 0 {
			cLess = true
		}
	}
	switch {
	case cLess && !cGreater:
		return -1
	case cGreater && !cLess:
		return 1
	default:
		return 0
	}
}

// Equal reports exact equality.
func (c Clock) Equal(other Clock) bool {
	if len(c) != len(other) {
		// Allow missing zero entries to still compare equal.
	}
	keys := map[string]struct{}{}
	for k := range c {
		keys[k] = struct{}{}
	}
	for k := range other {
		keys[k] = struct{}{}
	}
	for k := range keys {
		if c[k] != other[k] {
			return false
		}
	}
	return true
}

// Concurrent reports whether neither clock happened-before the other.
func (c Clock) Concurrent(other Clock) bool {
	return c.Compare(other) == 0 && !c.Equal(other)
}

// HappenedBefore reports c < other.
func (c Clock) HappenedBefore(other Clock) bool { return c.Compare(other) == -1 }

// String renders a stable debug representation.
func (c Clock) String() string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(" ")
		}
		short := k
		if len(short) > 8 {
			short = short[:8]
		}
		sb.WriteString(short + ":" + itoa(c[k]))
	}
	sb.WriteString("}")
	return sb.String()
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
