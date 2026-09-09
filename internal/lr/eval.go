package lr

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// RuntimeError is a script runtime failure with source position.
type RuntimeError struct {
	Msg string
	Pos Pos
}

func (e *RuntimeError) Error() string { return fmt.Sprintf("runtime error: %s at %s", e.Msg, e.Pos) }

// ---------- Environments ----------

// Env is a chained variable scope.
type Env struct {
	parent *Env
	vars   map[string]Value
	consts map[string]bool
}

// NewEnv creates a child scope (parent may be nil).
func NewEnv(parent *Env) *Env {
	return &Env{parent: parent, vars: map[string]Value{}, consts: map[string]bool{}}
}

// Get looks up a name through the chain.
func (e *Env) Get(name string) (Value, bool) {
	for s := e; s != nil; s = s.parent {
		if v, ok := s.vars[name]; ok {
			return v, true
		}
	}
	return Null, false
}

// Define binds a name in the current scope.
func (e *Env) Define(name string, v Value) { e.vars[name] = v }

// DefineConst binds an immutable name in the current scope.
func (e *Env) DefineConst(name string, v Value) {
	e.vars[name] = v
	e.consts[name] = true
}

// IsConst reports whether name is a const in this scope or any parent.
func (e *Env) IsConst(name string) bool {
	for s := e; s != nil; s = s.parent {
		if s.consts[name] {
			return true
		}
	}
	return false
}

// HasLocal reports whether name is bound in the current scope only.
func (e *Env) HasLocal(name string) bool {
	_, ok := e.vars[name]
	return ok
}

// Set assigns to the nearest existing binding, else defines in current scope.
func (e *Env) Set(name string, v Value) {
	for s := e; s != nil; s = s.parent {
		if _, ok := s.vars[name]; ok {
			s.vars[name] = v
			return
		}
	}
	e.vars[name] = v
}

// ---------- Control-flow signals ----------

type sigKind int

const (
	sigNone sigKind = iota
	sigReturn
	sigBreak
	sigContinue
)

type signal struct {
	kind sigKind
	val  Value
}

var noSignal = &signal{kind: sigNone}

// ---------- Evaluator ----------

// MaxCallDepth guards against runaway recursion.
const MaxCallDepth = 500

// Evaluator runs a parsed Program.
type Evaluator struct {
	Globals   *Env
	Rec       *Recorder
	Out       io.Writer
	WorkDir   string
	Args      []string
	Deadline  time.Time // zero = no timeout
	loopDepth int
	callDepth int
}

// NewEvaluator creates an evaluator with builtins installed.
func NewEvaluator(workDir string, args []string, out io.Writer, rec *Recorder) *Evaluator {
	ev := &Evaluator{
		Globals: NewEnv(nil),
		Rec:     rec,
		Out:     out,
		WorkDir: workDir,
		Args:    args,
	}
	ev.installBuiltins()
	ev.installStdlib()
	return ev
}

// tick enforces the run deadline (called per statement).
func (ev *Evaluator) tick(pos Pos) *RuntimeError {
	if !ev.Deadline.IsZero() && time.Now().After(ev.Deadline) {
		return &RuntimeError{Msg: "script timeout exceeded", Pos: pos}
	}
	return nil
}

func (ev *Evaluator) errf(pos Pos, format string, a ...any) *RuntimeError {
	return &RuntimeError{Msg: fmt.Sprintf(format, a...), Pos: pos}
}

// EvalProgram runs all statements. Top-level `return` ends the program.
func (ev *Evaluator) EvalProgram(p *Program) *RuntimeError {
	for _, s := range p.Stmts {
		if err := ev.tick(s.stmtPos()); err != nil {
			return err
		}
		sig, err := ev.evalStmt(s, ev.Globals)
		if err != nil {
			return err
		}
		switch sig.kind {
		case sigReturn:
			return nil
		case sigBreak:
			return ev.errf(s.stmtPos(), "'break' outside loop")
		case sigContinue:
			return ev.errf(s.stmtPos(), "'continue' outside loop")
		}
	}
	return nil
}

func (ev *Evaluator) evalBlock(b *BlockStmt, env *Env) (*signal, *RuntimeError) {
	child := NewEnv(env)
	for _, s := range b.Stmts {
		if err := ev.tick(s.stmtPos()); err != nil {
			return nil, err
		}
		sig, err := ev.evalStmt(s, child)
		if err != nil {
			return nil, err
		}
		if sig.kind != sigNone {
			return sig, nil
		}
	}
	return noSignal, nil
}

func (ev *Evaluator) evalStmt(s Stmt, env *Env) (*signal, *RuntimeError) {
	switch st := s.(type) {
	case *LetStmt:
		v, err := ev.evalExpr(st.Value, env)
		if err != nil {
			return nil, err
		}
		if st.Const {
			if env.HasLocal(st.Name) {
				return nil, ev.errf(st.Pos, "cannot redeclare %q (already defined in this scope)", st.Name)
			}
			env.DefineConst(st.Name, v)
		} else {
			if env.consts[st.Name] {
				return nil, ev.errf(st.Pos, "cannot redeclare const %q", st.Name)
			}
			env.Define(st.Name, v)
		}
		return noSignal, nil
	case *AssignStmt:
		v, err := ev.evalExpr(st.Value, env)
		if err != nil {
			return nil, err
		}
		return noSignal, ev.assign(st.Target, v, env)
	case *FnDecl:
		env.Define(st.Name, Value{Kind: KFunc, Fn: &Func{Name: st.Name, Params: st.Params, Body: st.Body, Closure: env}})
		return noSignal, nil
	case *IfStmt:
		c, err := ev.evalExpr(st.Cond, env)
		if err != nil {
			return nil, err
		}
		if c.Truthy() {
			return ev.evalBlock(st.Then, env)
		}
		if st.Else != nil {
			switch el := st.Else.(type) {
			case *BlockStmt:
				return ev.evalBlock(el, env)
			case *IfStmt:
				return ev.evalStmt(el, env)
			}
		}
		return noSignal, nil
	case *WhileStmt:
		ev.loopDepth++
		defer func() { ev.loopDepth-- }()
		for {
			c, err := ev.evalExpr(st.Cond, env)
			if err != nil {
				return nil, err
			}
			if !c.Truthy() {
				return noSignal, nil
			}
			sig, err := ev.evalBlock(st.Body, env)
			if err != nil {
				return nil, err
			}
			switch sig.kind {
			case sigReturn:
				return sig, nil
			case sigBreak:
				return noSignal, nil
			case sigContinue:
				continue
			}
		}
	case *ForStmt:
		iter, err := ev.evalExpr(st.Iter, env)
		if err != nil {
			return nil, err
		}
		items, err := ev.iterItems(iter, st.Pos)
		if err != nil {
			return nil, err
		}
		ev.loopDepth++
		defer func() { ev.loopDepth-- }()
		for _, item := range items {
			child := NewEnv(env)
			child.Define(st.Var, item)
			sig, err := ev.evalBlock(st.Body, child)
			if err != nil {
				return nil, err
			}
			switch sig.kind {
			case sigReturn:
				return sig, nil
			case sigBreak:
				return noSignal, nil
			case sigContinue:
				continue
			}
		}
		return noSignal, nil
	case *BreakStmt:
		if ev.loopDepth == 0 {
			return nil, ev.errf(st.Pos, "'break' outside loop")
		}
		return &signal{kind: sigBreak}, nil
	case *ContinueStmt:
		if ev.loopDepth == 0 {
			return nil, ev.errf(st.Pos, "'continue' outside loop")
		}
		return &signal{kind: sigContinue}, nil
	case *ReturnStmt:
		v := Null
		if st.Value != nil {
			var err *RuntimeError
			v, err = ev.evalExpr(st.Value, env)
			if err != nil {
				return nil, err
			}
		}
		return &signal{kind: sigReturn, val: v}, nil
	case *ExprStmt:
		_, err := ev.evalExpr(st.Expr, env)
		return noSignal, err
	case *BlockStmt:
		return ev.evalBlock(st, env)
	}
	return nil, ev.errf(s.stmtPos(), "unknown statement")
}

func (ev *Evaluator) iterItems(v Value, pos Pos) ([]Value, *RuntimeError) {
	switch v.Kind {
	case KList:
		return v.L, nil
	case KStr:
		out := make([]Value, 0, len(v.S))
		for _, r := range v.S {
			out = append(out, Str(string(r)))
		}
		return out, nil
	case KMap:
		keys := make([]string, 0, len(v.M))
		for k := range v.M {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic order
		out := make([]Value, 0, len(keys))
		for _, k := range keys {
			out = append(out, Str(k))
		}
		return out, nil
	}
	return nil, ev.errf(pos, "cannot iterate %s (need list, string or map)", v.TypeName())
}

func (ev *Evaluator) assign(target Expr, v Value, env *Env) *RuntimeError {
	switch t := target.(type) {
	case *IdentExpr:
		if env.IsConst(t.Name) {
			return ev.errf(t.Pos, "cannot assign to const %q", t.Name)
		}
		env.Set(t.Name, v)
		return nil
	case *IndexExpr:
		obj, err := ev.evalExpr(t.Obj, env)
		if err != nil {
			return err
		}
		idx, err := ev.evalExpr(t.Index, env)
		if err != nil {
			return err
		}
		switch obj.Kind {
		case KList:
			i, ok := AsInt(idx)
			if idx.Kind != KNum || !ok {
				return ev.errf(t.Pos, "list index must be a number")
			}
			if i < 0 || i >= int64(len(obj.L)) {
				return ev.errf(t.Pos, "list index %d out of range (len %d)", i, len(obj.L))
			}
			obj.L[i] = v
			return nil
		case KMap:
			if idx.Kind != KStr {
				return ev.errf(t.Pos, "map index must be a string")
			}
			obj.M[idx.S] = v
			return nil
		}
		return ev.errf(t.Pos, "cannot index-assign into %s", obj.TypeName())
	case *MemberExpr:
		obj, err := ev.evalExpr(t.Obj, env)
		if err != nil {
			return err
		}
		if obj.Kind != KMap {
			return ev.errf(t.Pos, "cannot set field on %s", obj.TypeName())
		}
		obj.M[t.Name] = v
		return nil
	}
	return ev.errf(target.exprPos(), "cannot assign to this expression")
}

// ---------- Expressions ----------

func (ev *Evaluator) evalExpr(e Expr, env *Env) (Value, *RuntimeError) {
	switch x := e.(type) {
	case *LitExpr:
		return x.Val, nil
	case *IdentExpr:
		v, ok := env.Get(x.Name)
		if !ok {
			return Null, ev.errf(x.Pos, "undefined variable %q", x.Name)
		}
		return v, nil
	case *BinaryExpr:
		return ev.evalBinary(x, env)
	case *UnaryExpr:
		return ev.evalUnary(x, env)
	case *CallExpr:
		return ev.evalCall(x, env)
	case *IndexExpr:
		return ev.evalIndex(x, env)
	case *MemberExpr:
		obj, err := ev.evalExpr(x.Obj, env)
		if err != nil {
			return Null, err
		}
		if obj.Kind == KNull {
			return Null, nil // nil-safe navigation
		}
		if obj.Kind != KMap {
			return Null, ev.errf(x.Pos, "%s has no fields", obj.TypeName())
		}
		return obj.Get(x.Name), nil
	case *ListExpr:
		out := make([]Value, 0, len(x.Elems))
		for _, el := range x.Elems {
			v, err := ev.evalExpr(el, env)
			if err != nil {
				return Null, err
			}
			out = append(out, v)
		}
		return List(out), nil
	case *MapExpr:
		m := map[string]Value{}
		for i, k := range x.Keys {
			v, err := ev.evalExpr(x.Vals[i], env)
			if err != nil {
				return Null, err
			}
			m[k] = v
		}
		return Value{Kind: KMap, M: m}, nil
	}
	return Null, ev.errf(e.exprPos(), "unknown expression")
}

func (ev *Evaluator) evalBinary(x *BinaryExpr, env *Env) (Value, *RuntimeError) {
	// Short-circuit logical operators (symbolic and word forms).
	switch x.Op {
	case T_AmpAmp, T_And:
		l, err := ev.evalExpr(x.Left, env)
		if err != nil {
			return Null, err
		}
		if !l.Truthy() {
			return l, nil
		}
		return ev.evalExpr(x.Right, env)
	case T_PipePipe, T_Or:
		l, err := ev.evalExpr(x.Left, env)
		if err != nil {
			return Null, err
		}
		if l.Truthy() {
			return l, nil
		}
		r, err := ev.evalExpr(x.Right, env)
		if err != nil {
			return Null, err
		}
		return r, nil
	}
	l, err := ev.evalExpr(x.Left, env)
	if err != nil {
		return Null, err
	}
	r, err := ev.evalExpr(x.Right, env)
	if err != nil {
		return Null, err
	}
	switch x.Op {
	case T_EqEq:
		return Bool(Equals(l, r)), nil
	case T_BangEq:
		return Bool(!Equals(l, r)), nil
	case T_In:
		return ev.evalIn(l, r, x.Pos)
	case T_Plus:
		if l.Kind == KNum && r.Kind == KNum {
			return Num(l.N + r.N), nil
		}
		if l.Kind == KStr || r.Kind == KStr {
			return Str(l.String() + r.String()), nil // easy concat
		}
		if l.Kind == KList && r.Kind == KList {
			out := make([]Value, 0, len(l.L)+len(r.L))
			return List(append(append(out, l.L...), r.L...)), nil
		}
		return Null, ev.errf(x.Pos, "cannot add %s and %s", l.TypeName(), r.TypeName())
	case T_Minus, T_Star, T_Slash, T_Percent:
		if l.Kind != KNum || r.Kind != KNum {
			return Null, ev.errf(x.Pos, "arithmetic needs numbers, got %s and %s", l.TypeName(), r.TypeName())
		}
		switch x.Op {
		case T_Minus:
			return Num(l.N - r.N), nil
		case T_Star:
			return Num(l.N * r.N), nil
		case T_Slash:
			if r.N == 0 {
				return Null, ev.errf(x.Pos, "division by zero")
			}
			return Num(l.N / r.N), nil
		case T_Percent:
			if r.N == 0 {
				return Null, ev.errf(x.Pos, "modulo by zero")
			}
			return Num(math.Mod(l.N, r.N)), nil
		}
	case T_Lt, T_LtEq, T_Gt, T_GtEq:
		cmp, ok := compare(l, r)
		if !ok {
			return Null, ev.errf(x.Pos, "cannot compare %s and %s", l.TypeName(), r.TypeName())
		}
		switch x.Op {
		case T_Lt:
			return Bool(cmp < 0), nil
		case T_LtEq:
			return Bool(cmp <= 0), nil
		case T_Gt:
			return Bool(cmp > 0), nil
		case T_GtEq:
			return Bool(cmp >= 0), nil
		}
	}
	return Null, ev.errf(x.Pos, "unknown operator")
}

func compare(a, b Value) (int, bool) {
	if a.Kind == KNum && b.Kind == KNum {
		switch {
		case a.N < b.N:
			return -1, true
		case a.N > b.N:
			return 1, true
		default:
			return 0, true
		}
	}
	if a.Kind == KStr && b.Kind == KStr {
		return strings.Compare(a.S, b.S), true
	}
	return 0, false
}

func (ev *Evaluator) evalIn(l, r Value, pos Pos) (Value, *RuntimeError) {
	switch r.Kind {
	case KList:
		for _, e := range r.L {
			if Equals(l, e) {
				return True, nil
			}
		}
		return False, nil
	case KMap:
		if l.Kind != KStr {
			return Null, ev.errf(pos, "'in' on a map needs a string key")
		}
		_, ok := r.M[l.S]
		return Bool(ok), nil
	case KStr:
		if l.Kind != KStr {
			return Null, ev.errf(pos, "'in' on a string needs a string")
		}
		return Bool(strings.Contains(r.S, l.S)), nil
	}
	return Null, ev.errf(pos, "cannot use 'in' on %s", r.TypeName())
}

func (ev *Evaluator) evalUnary(x *UnaryExpr, env *Env) (Value, *RuntimeError) {
	v, err := ev.evalExpr(x.X, env)
	if err != nil {
		return Null, err
	}
	switch x.Op {
	case T_Minus:
		if v.Kind != KNum {
			return Null, ev.errf(x.Pos, "unary '-' needs a number, got %s", v.TypeName())
		}
		return Num(-v.N), nil
	case T_Bang, T_Not:
		return Bool(!v.Truthy()), nil
	}
	return Null, ev.errf(x.Pos, "unknown unary operator")
}

func (ev *Evaluator) evalCall(x *CallExpr, env *Env) (Value, *RuntimeError) {
	fn, err := ev.evalExpr(x.Fn, env)
	if err != nil {
		return Null, err
	}
	args := make([]Value, 0, len(x.Args))
	for _, a := range x.Args {
		v, err := ev.evalExpr(a, env)
		if err != nil {
			return Null, err
		}
		args = append(args, v)
	}
	switch fn.Kind {
	case KBuiltin:
		return fn.Bl.Fn(ev, args, x.Pos)
	case KFunc:
		f := fn.Fn
		if len(args) != len(f.Params) {
			return Null, ev.errf(x.Pos, "%s takes %d argument(s), got %d", f.Name, len(f.Params), len(args))
		}
		ev.callDepth++
		if ev.callDepth > MaxCallDepth {
			ev.callDepth--
			return Null, ev.errf(x.Pos, "call stack overflow (deep recursion?)")
		}
		callEnv := NewEnv(f.Closure)
		for i, p := range f.Params {
			callEnv.Define(p, args[i])
		}
		sig, runErr := ev.evalBlock(f.Body, callEnv)
		ev.callDepth--
		if runErr != nil {
			return Null, runErr
		}
		if sig.kind == sigReturn {
			return sig.val, nil
		}
		if sig.kind != sigNone {
			return Null, ev.errf(x.Pos, "misplaced '%s'", sigName(sig.kind))
		}
		return Null, nil
	}
	return Null, ev.errf(x.Pos, "%s is not callable", fn.TypeName())
}

func sigName(k sigKind) string {
	if k == sigBreak {
		return "break"
	}
	return "continue"
}

func (ev *Evaluator) evalIndex(x *IndexExpr, env *Env) (Value, *RuntimeError) {
	obj, err := ev.evalExpr(x.Obj, env)
	if err != nil {
		return Null, err
	}
	idx, err := ev.evalExpr(x.Index, env)
	if err != nil {
		return Null, err
	}
	switch obj.Kind {
	case KList:
		i, _ := AsInt(idx)
		if idx.Kind != KNum {
			return Null, ev.errf(x.Pos, "list index must be a number")
		}
		if i < 0 || i >= int64(len(obj.L)) {
			return Null, ev.errf(x.Pos, "list index %d out of range (len %d)", i, len(obj.L))
		}
		return obj.L[i], nil
	case KMap:
		if idx.Kind != KStr {
			return Null, ev.errf(x.Pos, "map index must be a string")
		}
		return obj.Get(idx.S), nil
	case KStr:
		i, _ := AsInt(idx)
		if idx.Kind != KNum {
			return Null, ev.errf(x.Pos, "string index must be a number")
		}
		runes := []rune(obj.S)
		if i < 0 || i >= int64(len(runes)) {
			return Null, ev.errf(x.Pos, "string index %d out of range (len %d)", i, len(runes))
		}
		return Str(string(runes[i])), nil
	}
	return Null, ev.errf(x.Pos, "cannot index into %s", obj.TypeName())
}
