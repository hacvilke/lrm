package lr

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWhileHonoursDeadlineWithEmptyBody locks the fix for an empty-bodied
// loop: `while true { }` used to reach its deadline check only through
// statements inside the body, so with no body it never checked at all and
// hung the process forever. Every iteration must tick.
func TestWhileHonoursDeadlineWithEmptyBody(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "spin.lr")
	if err := os.WriteFile(script, []byte("print(\"spinning\");\nwhile true { }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}, Timeout: 150 * time.Millisecond})
	elapsed := time.Since(start)
	if res.Passed {
		t.Fatalf("run must fail on timeout, got %+v", res)
	}
	if !strings.Contains(res.Error, "timeout") {
		t.Fatalf("want timeout error, got %q", res.Error)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("run took %s — the loop is not checking the deadline", elapsed)
	}
}

// TestWhileEmptyBodyConditionOnly covers the same hole via a condition that
// never changes, with a body comment instead of a statement.
func TestWhileEmptyBodyConditionOnly(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "spin2.lr")
	src := "let n = 1;\nwhile n == 1 {\n  // nothing to do, forever\n}\n"
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}, Timeout: 150 * time.Millisecond})
	if res.Passed {
		t.Fatalf("run must fail on timeout, got %+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("run took %s — deadline not enforced", time.Since(start))
	}
}

// TestUTF8BOMIsTolerated: Windows editors (Notepad, PowerShell redirection)
// write a BOM. It must not become "unexpected character".
func TestUTF8BOMIsTolerated(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bom.lr")
	var out bytes.Buffer
	body := "\ufeffprint(\"bom ok\");\r\nlet x = 2;\r\nassert(x == 2, \"crlf + bom\");\r\n"
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res := RunFile(script, Options{WorkDir: dir, Out: &out, Timeout: 5 * time.Second})
	if !res.Passed {
		t.Fatalf("BOM+CRLF script must pass: %+v err=%s", res, res.Error)
	}
	if !strings.Contains(out.String(), "bom ok") {
		t.Fatalf("output missing: %q", out.String())
	}
}

// TestMissingSemiErrorNamesTheLine: the parser cannot see "statement ended"
// without a ';', so it must at least point at the line that forgot one.
func TestMissingSemiErrorNamesTheLine(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "nosemi.lr")
	if err := os.WriteFile(script, []byte("let a = 1\nlet b = 2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}, Timeout: 5 * time.Second})
	if res.Passed {
		t.Fatal("missing ';' must fail to parse")
	}
	if !strings.Contains(res.Error, "line 1") || !strings.Contains(res.Error, "newlines are not terminators") {
		t.Fatalf("error does not explain the missing terminator: %q", res.Error)
	}
}

// TestAnonymousFunctions runs lambdas the way a user would: as values,
// passed to another function, returned from one (real closure), stored in a
// list per loop iteration, and recursive through its own binding.
func TestAnonymousFunctions(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fns.lr")
	src := `
let double = fn(x) { return x * 2; };
assert(type(double) == "fn", "lambda is a fn value");
assert(double(21) == 42, "call through a variable");

fn apply(f, v) { return f(v); }
assert(apply(fn(s) { return upper(s); }, "ok") == "OK", "lambda as an argument");

fn make_adder(n) { return fn(x) { return x + n; }; }
let add5 = make_adder(5);
assert(add5(1) == 6, "returned lambda keeps its captured environment");

let counter = 0;
let bump = fn() { counter = counter + 1; return counter; };
bump(); bump();
assert(counter == 2, "lambda mutates the captured variable");

let fns = [];
for i in [1, 2, 3] { fns = fns + [fn() { return i; }]; }
assert(fns[0]() == 1 and fns[1]() == 2 and fns[2]() == 3, "one closure per iteration");

let fact = fn(n) { if n < 2 { return 1; } return n * fact(n - 1); };
assert(fact(5) == 120, "lambda can recurse through its own binding");

assert((fn(a, b) { return a + b; })(2, 3) == 5, "immediately invoked lambda");
assert(str(fn(a) { return a; }) == "<fn>", "anonymous values print as <fn>");
`
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res := RunFile(script, Options{WorkDir: dir, Out: &out, Timeout: 10 * time.Second})
	if !res.Passed {
		t.Fatalf("lambda suite failed: %+v err=%s out=%s", res, res.Error, out.String())
	}
	if res.AssertsPass < 9 {
		t.Fatalf("expected the whole suite to run, only %d asserts passed", res.AssertsPass)
	}
}
