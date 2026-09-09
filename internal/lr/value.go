// Package lr implements LRS — the LRM Script runtime language.
//
// LRS (.lr files) is an imperative, Rust-flavored but easy-syntax language
// for automating LRM sharing and testing site/network behavior:
//
//	$ lrm run check.lr --report out.txt
//	$ lrm check.lr                      # bare form also works
//
// Every run exports a .txt report: timestamped logs, network events,
// assertion results, and — on failure — the exact script error.
package lr

import (
	"fmt"
	"sort"
	"strconv"
)

// Kind identifies a Value's runtime type.
type Kind int

const (
	KNull Kind = iota
	KBool
	KNum
	KStr
	KList
	KMap
	KFunc
	KBuiltin
)

// Value is one LRS runtime value. Numbers are float64 (printed as integers
// when integral); maps have string keys.
type Value struct {
	Kind Kind
	B    bool
	N    float64
	S    string
	L    []Value
	M    map[string]Value
	Fn   *Func
	Bl   *Builtin
}

// Func is a user-defined function value (with its defining closure).
type Func struct {
	Name    string
	Params  []string
	Body    *BlockStmt
	Closure *Env
}

// Builtin is a native function value.
type Builtin struct {
	Name string
	Fn   func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError)
}

var (
	Null  = Value{Kind: KNull}
	True  = Value{Kind: KBool, B: true}
	False = Value{Kind: KBool, B: false}
)

// Constructors.
func Num(n float64) Value { return Value{Kind: KNum, N: n} }
func Int(n int64) Value   { return Value{Kind: KNum, N: float64(n)} }
func Str(s string) Value  { return Value{Kind: KStr, S: s} }
func Bool(b bool) Value {
	if b {
		return True
	}
	return False
}
func List(v []Value) Value { return Value{Kind: KList, L: v} }
func NewMap() Value        { return Value{Kind: KMap, M: map[string]Value{}} }

// TypeName returns the LRS type name (for type() and error messages).
func (v Value) TypeName() string {
	switch v.Kind {
	case KNull:
		return "null"
	case KBool:
		return "bool"
	case KNum:
		return "num"
	case KStr:
		return "string"
	case KList:
		return "list"
	case KMap:
		return "map"
	case KFunc:
		return "fn"
	case KBuiltin:
		return "builtin"
	}
	return "unknown"
}

// Truthy: only null and false are falsy; everything else (including 0 and
// "") is truthy. One rule, no surprises.
func (v Value) Truthy() bool {
	switch v.Kind {
	case KNull:
		return false
	case KBool:
		return v.B
	}
	return true
}

// String renders a Value for print()/reports. Strings print raw (no quotes);
// numbers print as integers when integral.
func (v Value) String() string {
	switch v.Kind {
	case KNull:
		return "null"
	case KBool:
		if v.B {
			return "true"
		}
		return "false"
	case KNum:
		if v.N == float64(int64(v.N)) && v.N < 9e15 && v.N > -9e15 {
			return strconv.FormatInt(int64(v.N), 10)
		}
		return strconv.FormatFloat(v.N, 'g', -1, 64)
	case KStr:
		return v.S
	case KList:
		parts := make([]string, 0, len(v.L))
		for _, e := range v.L {
			parts = append(parts, e.repr())
		}
		return "[" + joinStrings(parts, ", ") + "]"
	case KMap:
		keys := make([]string, 0, len(v.M))
		for k := range v.M {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%q: %s", k, v.M[k].repr()))
		}
		return "{" + joinStrings(parts, ", ") + "}"
	case KFunc:
		return fmt.Sprintf("<fn %s>", v.Fn.Name)
	case KBuiltin:
		return fmt.Sprintf("<builtin %s>", v.Bl.Name)
	}
	return "?"
}

// repr is String but quotes strings (for nested display).
func (v Value) repr() string {
	if v.Kind == KStr {
		return strconv.Quote(v.S)
	}
	return v.String()
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// Equals compares two values (deep for lists/maps).
func Equals(a, b Value) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KNull:
		return true
	case KBool:
		return a.B == b.B
	case KNum:
		return a.N == b.N
	case KStr:
		return a.S == b.S
	case KList:
		if len(a.L) != len(b.L) {
			return false
		}
		for i := range a.L {
			if !Equals(a.L[i], b.L[i]) {
				return false
			}
		}
		return true
	case KMap:
		if len(a.M) != len(b.M) {
			return false
		}
		for k, av := range a.M {
			bv, ok := b.M[k]
			if !ok || !Equals(av, bv) {
				return false
			}
		}
		return true
	case KFunc:
		return a.Fn == b.Fn
	case KBuiltin:
		return a.Bl == b.Bl
	}
	return false
}

// AsInt converts a num Value to int64 (error unless integral-ish).
func AsInt(v Value) (int64, bool) {
	if v.Kind != KNum {
		return 0, false
	}
	return int64(v.N), true
}

// Get fetches a map key (Null when missing — friendly for r.ok probing).
func (v Value) Get(key string) Value {
	if v.Kind == KMap {
		if val, ok := v.M[key]; ok {
			return val
		}
	}
	return Null
}
