package lr

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lrm-project/lrm/internal/store"
)

func initRepo(t *testing.T, dir string) {
	t.Helper()
	r, err := store.Init(dir, "tester", 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
}

// evalSrc parses + runs src with a fresh evaluator, returning output.
func evalSrc(t *testing.T, src string) (*Evaluator, *Recorder, *bytes.Buffer) {
	t.Helper()
	prog, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	rec := NewRecorder()
	ev := NewEvaluator(t.TempDir(), nil, &buf, rec)
	if err := ev.EvalProgram(prog); err != nil {
		t.Fatalf("eval: %v", err)
	}
	return ev, rec, &buf
}

func mustParseFail(t *testing.T, src string) {
	t.Helper()
	if _, err := Parse(src); err == nil {
		t.Fatalf("expected parse error for %q", src)
	}
}

func mustEvalFail(t *testing.T, src string) {
	t.Helper()
	prog, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ev := NewEvaluator(t.TempDir(), nil, &bytes.Buffer{}, NewRecorder())
	if err := ev.EvalProgram(prog); err == nil {
		t.Fatalf("expected runtime error for %q", src)
	}
}

func TestLexerBasics(t *testing.T) {
	toks, err := Lex(`let x = 12.5 + "a\nb"; // hi
/* multi
line */ if x >= 2 and not false {}`)
	if err != nil {
		t.Fatal(err)
	}
	types := []Tok{}
	for _, tk := range toks {
		types = append(types, tk.Type)
	}
	want := []Tok{T_Let, T_Ident, T_Eq, T_Num, T_Plus, T_Str, T_Semi, T_If, T_Ident, T_GtEq, T_Num, T_And, T_Not, T_False, T_LBrace, T_RBrace, T_EOF}
	if len(types) != len(want) {
		t.Fatalf("toks=%v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("tok %d = %v want %v", i, types[i], want[i])
		}
	}
	if toks[5].Text != "a\nb" {
		t.Fatalf("string escape: %q", toks[5].Text)
	}
}

func TestLexerErrors(t *testing.T) {
	for _, src := range []string{`"abc`, "'ab\nc'", "`x`", "/* never closed", `"bad \q esc"`} {
		if _, err := Lex(src); err == nil {
			t.Fatalf("expected lex error for %q", src)
		}
	}
}

func TestArithmeticAndPrecedence(t *testing.T) {
	_, _, buf := evalSrc(t, `print(1 + 2 * 3); print((1 + 2) * 3); print(10 / 4); print(10 % 3);`)
	if buf.String() != "7\n9\n2.5\n1\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestStringsListsMaps(t *testing.T) {
	_, _, buf := evalSrc(t, `
let s = "he" + "llo";
print(s);
print(len(s));
let l = [1, 2] + [3];
print(l[2]);
l[0] = 9;
print(l);
let m = {"a": 1, "b": 2};
print(m.b);
print(m["zz"]);
m.c = 3;
print(len(m));
for k in m { print(k); }
print("ell" in "hello");
print(2 in l);
print("a" in m);
`)
	want := "hello\n5\n3\n[9, 2, 3]\n2\nnull\n3\na\nb\nc\ntrue\ntrue\ntrue\n"
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestControlFlow(t *testing.T) {
	_, _, buf := evalSrc(t, `
let n = 0;
for i in [1, 2, 3, 4] {
  if i == 3 { continue; }
  if i == 4 { break; }
  n = n + i;
}
print(n);
let w = 0;
while w < 3 { w = w + 1; }
print(w);
if n == 3 { print("three"); } else { print("other"); }
`)
	if buf.String() != "3\n3\nthree\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestFunctionsAndClosures(t *testing.T) {
	_, _, buf := evalSrc(t, `
fn add(a, b) { return a + b; }
print(add(2, 3));
fn counter() {
  let n = 0;
  fn inc() { n = n + 1; return n; }
  return inc;
}
let c = counter();
print(c());
print(c());
fn noreturn() { let x = 1; }
print(noreturn());
`)
	if buf.String() != "5\n1\n2\nnull\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestLogicAndCompare(t *testing.T) {
	_, _, buf := evalSrc(t, `
print(true and false);
print(true or false);
print(not false);
print(1 < 2 and 3 >= 3);
print("a" < "b");
print(null == null);
print([1] == [1]);
`)
	if buf.String() != "false\ntrue\ntrue\ntrue\ntrue\ntrue\ntrue\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestBuiltins(t *testing.T) {
	_, _, buf := evalSrc(t, `
print(split("a,b,c", ","));
print(join(["x", "y"], "-"));
print(contains("hello", "ll"));
print(trim("  hi  "));
print(upper("ab") + lower("CD"));
print(int("42") + int(3.9));
print(type(1) + "," + type("s") + "," + type(null));
print(str([1, 2]));
sleep_ms(1);
print(now_ms() > 0);
print(args());
`)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{`["a", "b", "c"]`, "x-y", "true", "hi", "ABcd", "45", "num,string,null", "[1, 2]", "true", "[]"}
	if len(lines) != len(want) {
		t.Fatalf("lines=%v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q want %q", i, lines[i], want[i])
		}
	}
}

func TestAssertSoftAndReport(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "t.lr")
	_ = os.WriteFile(script, []byte(`
log("hello assert");
assert(1 + 1 == 2, "math works");
assert(false, "deliberate fail");
print("still running");
`), 0o644)
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if res.Passed {
		t.Fatal("should fail (one bad assert)")
	}
	if res.AssertsPass != 1 || res.AssertsFail != 1 {
		t.Fatalf("asserts %+v", res)
	}
	raw, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, needle := range []string{"verdict: FAIL", "PASS math works", "FAIL deliberate fail", "still running", "hello assert"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("report missing %q:\n%s", needle, body)
		}
	}
}

func TestErrorReportOnSyntaxError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bad.lr")
	_ = os.WriteFile(script, []byte(`let x = ;`), 0o644)
	var buf bytes.Buffer
	res := RunFile(script, Options{WorkDir: dir, Out: &buf})
	if res.Passed || res.Error == "" {
		t.Fatalf("res=%+v", res)
	}
	raw, _ := os.ReadFile(res.ReportPath)
	if !strings.Contains(string(raw), "verdict: FAIL") || !strings.Contains(string(raw), "syntax error") {
		t.Fatalf("bad error report:\n%s", raw)
	}
}

func TestErrorReportOnRuntimeError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "rt.lr")
	_ = os.WriteFile(script, []byte("print(nosuchvar);"), 0o644)
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if res.Passed {
		t.Fatal("should fail")
	}
	raw, _ := os.ReadFile(res.ReportPath)
	if !strings.Contains(string(raw), "undefined variable") {
		t.Fatalf("report:\n%s", raw)
	}
}

func TestRuntimeErrors(t *testing.T) {
	mustEvalFail(t, `print(undefined_var);`)
	mustEvalFail(t, `1 + true;`)
	mustEvalFail(t, `1 / 0;`)
	mustEvalFail(t, `let l = [1]; print(l[5]);`)
	mustEvalFail(t, `break;`)
	mustEvalFail(t, `fn f(a) { return a; } f(1, 2);`)
	mustEvalFail(t, `fail("boom");`)
	mustEvalFail(t, `for x in 42 { print(x); }`)
	mustParseFail(t, `let x = ;`)
	mustParseFail(t, `if true print(1);`)
	mustParseFail(t, `fn (x) { return x; }`)
}

func TestFileSandbox(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "f.lr")
	_ = os.WriteFile(script, []byte(`
write_file("sub/note.txt", "hi");
let r = read_file("sub/note.txt");
assert(r.ok and r.text == "hi", "roundtrip");
assert(file_exists("sub/note.txt"), "exists");
let bad = read_file("../escape.txt");
assert(bad.ok == false, "escape blocked");
let abs = read_file("/etc/hostname");
assert(abs.ok == false, "absolute blocked");
`), 0o644)
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if !res.Passed {
		t.Fatalf("res=%+v err=%s", res, res.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "../escape.txt")); err == nil {
		t.Fatal("sandbox escape wrote outside!")
	}
}

func TestTimeout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "loop.lr")
	_ = os.WriteFile(script, []byte(`while true { let x = 1; }`), 0o644)
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}, Timeout: 300 * time.Millisecond})
	if res.Passed || !strings.Contains(res.Error, "timeout") {
		t.Fatalf("res=%+v", res)
	}
}

func TestTCPLocal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	script := filepath.Join(dir, "tcp.lr")
	_ = os.WriteFile(script, []byte(`
let r = tcp_connect("127.0.0.1", PORT, 2000);
assert(r.ok, "local tcp connects");
assert(r.latency_ms >= 0, "latency present");
let bad = tcp_connect("127.0.0.1", 1, 300);
assert(bad.ok == false, "closed port fails cleanly");
`), 0o644)
	raw, _ := os.ReadFile(script)
	_ = os.WriteFile(script, []byte(strings.ReplaceAll(string(raw), "PORT", itoa(port))), 0o644)
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if !res.Passed {
		t.Fatalf("res=%+v err=%s", res, res.Error)
	}
	rep, _ := os.ReadFile(res.ReportPath)
	if !strings.Contains(string(rep), "tcp 127.0.0.1") {
		t.Fatalf("no NET lines:\n%s", rep)
	}
}

func TestHTTPLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok-body"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	script := filepath.Join(dir, "h.lr")
	_ = os.WriteFile(script, []byte(`let r = http_get(URL, 3000); assert(r.ok and r.status == 200, "http 200");`), 0o644)
	raw, _ := os.ReadFile(script)
	_ = os.WriteFile(script, []byte(strings.ReplaceAll(string(raw), "URL", `"`+srv.URL+`"`)), 0o644)
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if !res.Passed {
		t.Fatalf("res=%+v err=%s", res, res.Error)
	}
}

func TestLRMBuiltinLocal(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "repo.lr")
	// Run outside a repo: builtins must fail gracefully with {ok:false}.
	_ = os.WriteFile(script, []byte(`
let s = lrm_status();
assert(s.ok == false, "status outside repo fails cleanly");
`), 0o644)
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if !res.Passed {
		t.Fatalf("res=%+v err=%s", res, res.Error)
	}
	// Inside a temp repo: status/commit/log roundtrip.
	proj := filepath.Join(dir, "proj")
	_ = os.WriteFile(filepath.Join(dir, "repo2.lr"), []byte(`
write_file("a.txt", "v1");
let c = lrm_commit("first via lr");
assert(c.ok, "commit works");
let s = lrm_status();
assert(s.clean and s.branch == "main", "clean on main");
let l = lrm_log(5);
assert(len(l.commits) == 1, "one commit");
`), 0o644)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// init repo via LRM store directly
	initRepo(t, proj)
	res2 := RunFile(filepath.Join(dir, "repo2.lr"), Options{WorkDir: proj, Out: &bytes.Buffer{}})
	if !res2.Passed {
		t.Fatalf("res=%+v err=%s", res2, res2.Error)
	}
}

func itoa(n int) string {
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

func TestCompoundAssign(t *testing.T) {
	_, _, buf := evalSrc(t, `
let x = 10;
x += 1; x -= 2; x *= 3; x /= 3; x %= 8;
print(x);
let m = {a: 2};
m.a += 3;
print(m.a);
let l = [4];
l[0] *= 2;
print(l[0]);
let s = "a";
s += "b";
print(s);
`)
	if buf.String() != "1\n5\n8\nab\n" {
		t.Fatalf("got %q", buf.String())
	}
	mustEvalFail(t, `y += 1;`) // compound assign reads y first: undefined variable
}

func TestConst(t *testing.T) {
	_, _, buf := evalSrc(t, `const C = 41; print(C + 1);`)
	if buf.String() != "42\n" {
		t.Fatalf("got %q", buf.String())
	}
	mustEvalFail(t, `const C = 1; C = 2;`)
	mustEvalFail(t, `const C = 1; const C = 2;`)
	mustEvalFail(t, `const C = 1; let C = 2;`)
	mustEvalFail(t, `const C = 1; C += 1;`)
}

func TestAndOrOperandValues(t *testing.T) {
	_, _, buf := evalSrc(t, `
print(false and "x");
print(true and "x");
print(false or "x");
print(true or "x");
print(0 or "dflt");
print(true && 7);
print(false || 8);
print(!false);
`)
	if buf.String() != "false\nx\nx\ntrue\n0\n7\n8\ntrue\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestForOverString(t *testing.T) {
	_, _, buf := evalSrc(t, `
let out = "";
for c in "hé" { out += c + ","; }
print(out);
`)
	if buf.String() != "h,é,\n" {
		t.Fatalf("got %q", buf.String())
	}
	mustEvalFail(t, `for x in 42 { print(x); }`)
}
