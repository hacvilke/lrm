package lr

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// installBuiltins registers pure (non-IO, non-LRM) native functions.
// File, network and LRM builtins live in stdlib.go.
func (ev *Evaluator) installBuiltins() {
	reg := func(name string, fn func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError)) {
		ev.Globals.Define(name, Value{Kind: KBuiltin, Bl: &Builtin{Name: name, Fn: fn}})
	}

	reg("print", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		s := joinValues(args)
		fmt.Fprintln(ev.Out, s)
		ev.Rec.Out(s)
		return Null, nil
	})
	reg("log", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		s := joinValues(args)
		fmt.Fprintf(ev.Out, "[%s] %s\n", time.Now().Format("15:04:05"), s)
		ev.Rec.Log(s)
		return Null, nil
	})
	reg("len", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("len", args, 1, 1, pos); err != nil {
			return Null, err
		}
		switch args[0].Kind {
		case KStr:
			return Int(int64(len([]rune(args[0].S)))), nil
		case KList:
			return Int(int64(len(args[0].L))), nil
		case KMap:
			return Int(int64(len(args[0].M))), nil
		}
		return Null, ev.errf(pos, "len() needs string, list or map, got %s", args[0].TypeName())
	})
	reg("str", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("str", args, 1, 1, pos); err != nil {
			return Null, err
		}
		return Str(args[0].String()), nil
	})
	reg("int", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("int", args, 1, 1, pos); err != nil {
			return Null, err
		}
		switch v := args[0]; v.Kind {
		case KNum:
			return Int(int64(v.N)), nil
		case KBool:
			if v.B {
				return Int(1), nil
			}
			return Int(0), nil
		case KStr:
			n, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64)
			if err != nil {
				return Null, ev.errf(pos, "int() cannot parse %q", v.S)
			}
			return Int(int64(n)), nil
		}
		return Null, ev.errf(pos, "int() needs num, bool or string, got %s", args[0].TypeName())
	})
	reg("type", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("type", args, 1, 1, pos); err != nil {
			return Null, err
		}
		return Str(args[0].TypeName()), nil
	})
	reg("split", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("split", args, 2, 2, pos); err != nil {
			return Null, err
		}
		s, sep, err := twoStrs(ev, "split", args, pos)
		if err != nil {
			return Null, err
		}
		parts := strings.Split(s, sep)
		out := make([]Value, 0, len(parts))
		for _, p := range parts {
			out = append(out, Str(p))
		}
		return List(out), nil
	})
	reg("join", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("join", args, 2, 2, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KList || args[1].Kind != KStr {
			return Null, ev.errf(pos, "join() needs (list, string)")
		}
		parts := make([]string, 0, len(args[0].L))
		for _, e := range args[0].L {
			parts = append(parts, e.String())
		}
		return Str(strings.Join(parts, args[1].S)), nil
	})
	reg("contains", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("contains", args, 2, 2, pos); err != nil {
			return Null, err
		}
		hay, needle := args[0], args[1]
		switch hay.Kind {
		case KStr:
			if needle.Kind != KStr {
				return Null, ev.errf(pos, "contains(string, string) — needle must be a string")
			}
			return Bool(strings.Contains(hay.S, needle.S)), nil
		case KList:
			for _, e := range hay.L {
				if Equals(e, needle) {
					return True, nil
				}
			}
			return False, nil
		case KMap:
			if needle.Kind != KStr {
				return Null, ev.errf(pos, "contains(map, string) — needle must be a string")
			}
			_, ok := hay.M[needle.S]
			return Bool(ok), nil
		}
		return Null, ev.errf(pos, "contains() needs string, list or map, got %s", hay.TypeName())
	})
	reg("keys", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("keys", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KMap {
			return Null, ev.errf(pos, "keys() needs a map, got %s", args[0].TypeName())
		}
		ks := make([]string, 0, len(args[0].M))
		for k := range args[0].M {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		out := make([]Value, 0, len(ks))
		for _, k := range ks {
			out = append(out, Str(k))
		}
		return List(out), nil
	})
	reg("trim", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("trim", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "trim() needs a string")
		}
		return Str(strings.TrimSpace(args[0].S)), nil
	})
	reg("upper", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("upper", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "upper() needs a string")
		}
		return Str(strings.ToUpper(args[0].S)), nil
	})
	reg("lower", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("lower", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "lower() needs a string")
		}
		return Str(strings.ToLower(args[0].S)), nil
	})
	reg("sleep_ms", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("sleep_ms", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KNum || args[0].N < 0 {
			return Null, ev.errf(pos, "sleep_ms() needs a non-negative number")
		}
		time.Sleep(time.Duration(args[0].N * float64(time.Millisecond)))
		return Null, nil
	})
	reg("now_ms", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("now_ms", args, 0, 0, pos); err != nil {
			return Null, err
		}
		return Int(time.Now().UnixMilli()), nil
	})
	reg("args", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("args", args, 0, 0, pos); err != nil {
			return Null, err
		}
		out := make([]Value, 0, len(ev.Args))
		for _, a := range ev.Args {
			out = append(out, Str(a))
		}
		return List(out), nil
	})
	// assert(cond [, msg]): soft assertion — records PASS/FAIL and CONTINUES,
	// so one run collects every failure. Final verdict FAIL if any failed.
	reg("assert", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("assert", args, 1, 2, pos); err != nil {
			return Null, err
		}
		msg := "assertion"
		if len(args) == 2 {
			if args[1].Kind != KStr {
				return Null, ev.errf(pos, "assert() message must be a string")
			}
			msg = args[1].S
		}
		pass := args[0].Truthy()
		ev.Rec.Assert(pass, msg, pos)
		mark := "PASS"
		if !pass {
			mark = "FAIL"
		}
		fmt.Fprintf(ev.Out, "[assert] %s: %s\n", mark, msg)
		return Bool(pass), nil
	})
	// fail(msg): hard abort — stops the script immediately with an error.
	reg("fail", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("fail", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "fail() needs a string message")
		}
		return Null, ev.errf(pos, "fail(): %s", args[0].S)
	})
}

// ---------- helpers shared with stdlib.go ----------

func joinValues(args []Value) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, " ")
}

func arity(name string, args []Value, min, max int, pos Pos) *RuntimeError {
	if len(args) < min || len(args) > max {
		want := fmt.Sprintf("%d", min)
		if min != max {
			want = fmt.Sprintf("%d..%d", min, max)
		}
		return &RuntimeError{Msg: fmt.Sprintf("%s() takes %s argument(s), got %d", name, want, len(args)), Pos: pos}
	}
	return nil
}

func twoStrs(ev *Evaluator, name string, args []Value, pos Pos) (string, string, *RuntimeError) {
	if args[0].Kind != KStr || args[1].Kind != KStr {
		return "", "", ev.errf(pos, "%s() needs strings", name)
	}
	return args[0].S, args[1].S, nil
}

func okMap(pairs ...any) Value {
	m := NewMap()
	m.M["ok"] = True
	for i := 0; i+1 < len(pairs); i += 2 {
		if k, ok := pairs[i].(string); ok {
			m.M[k] = toValue(pairs[i+1])
		}
	}
	return m
}

func errMap(msg string) Value {
	m := NewMap()
	m.M["ok"] = False
	m.M["error"] = Str(msg)
	return m
}

func toValue(v any) Value {
	switch t := v.(type) {
	case nil:
		return Null
	case bool:
		return Bool(t)
	case string:
		return Str(t)
	case int:
		return Int(int64(t))
	case int64:
		return Int(t)
	case float64:
		return Num(t)
	case []string:
		out := make([]Value, 0, len(t))
		for _, s := range t {
			out = append(out, Str(s))
		}
		return List(out)
	case []Value:
		return List(t)
	case map[string]string:
		m := NewMap()
		for k, v := range t {
			m.M[k] = Str(v)
		}
		return m
	case Value:
		return t
	}
	return Str(fmt.Sprintf("%v", v))
}
