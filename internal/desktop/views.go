package desktop

import (
	"fmt"
	"html"
	"strings"
)

// renderPage renders the whole panel. One page, no JavaScript frameworks, no
// external assets: the app must work on a machine with no internet, which is
// exactly the machine LRM is for.
func renderPage(st State, token string, checks []Check) []byte {
	var b strings.Builder
	e := html.EscapeString

	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>LRM</title><style>`)
	b.WriteString(css)
	b.WriteString(`</style></head><body>`)

	// header
	b.WriteString(`<header><h1>LRM</h1><span class="pill">` + e(st.Version) + `</span>
<span class="sub">` + e(st.GOOS) + `/` + e(st.GOARCH) + ` · ` + e(st.Home) + `</span></header>`)

	b.WriteString(`<main>`)

	if kind, msg := st.FlashKind, st.Flash; msg != "" {
		b.WriteString(`<div class="flash ` + e(kind) + `"><pre>` + e(msg) + `</pre></div>`)
	}

	// --- workspace card -------------------------------------------------
	b.WriteString(`<section><h2>Workspace</h2>`)
	if !st.Repo.Exists {
		b.WriteString(`<p class="muted">No workspace here yet. A workspace is the folder LRM keeps in sync — the thing a repository is to git, except it is shared live with the people you invite.</p>`)
		if st.Repo.RefusalReason != "" {
			b.WriteString(`<p class="warn">` + e(st.Repo.RefusalReason) + `</p>`)
		}
		b.WriteString(`<div class="grid">`)
		// create
		b.WriteString(`<form method="post" action="/action"><h3>Create one here</h3>
<input type="hidden" name="t" value="` + e(token) + `"><input type="hidden" name="action" value="init">
<label>Name (shown to your team)<input name="user" placeholder="ada" required></label>
<label>Folder<input name="dir" value="` + e(st.Repo.Path) + `" required></label>
<button>Create workspace</button></form>`)
		// join
		b.WriteString(`<form method="post" action="/action"><h3>Join a teammate</h3>
<input type="hidden" name="t" value="` + e(token) + `"><input type="hidden" name="action" value="clone">
<label>Port Key<input name="key" placeholder="[192.168.1.20]:[8443]" required></label>
<label>Folder<input name="dir" placeholder="~/work/project" required></label>
<label>Your name<input name="user" placeholder="ada"></label>
<button>Join workspace</button></form>`)
		b.WriteString(`</div>`)
	} else {
		b.WriteString(`<dl>`)
		b.WriteString(row("Path", st.Repo.Path))
		b.WriteString(row("You", st.Repo.User))
		b.WriteString(row("Device", st.Repo.PeerID+"  ·  key "+st.Repo.Fingerprint))
		b.WriteString(row("Workspace ID", st.Repo.Workspace))
		b.WriteString(row("Port", fmt.Sprint(st.Repo.Port)))
		b.WriteString(row("HEAD", st.Repo.HeadBranch+" @ "+st.Repo.HeadCommit+"  ("+fmt.Sprint(st.Repo.Commits)+" commits)"))
		b.WriteString(`</dl>`)
		b.WriteString(`<div class="row2">`)
		b.WriteString(`<form method="post" action="/action" class="inline">
<input type="hidden" name="t" value="` + e(token) + `"><input type="hidden" name="action" value="user">
<input name="user" value="` + e(st.Repo.User) + `" size="16"><button>Rename me</button></form>`)
		b.WriteString(`<form method="post" action="/action" class="inline">
<input type="hidden" name="t" value="` + e(token) + `"><input type="hidden" name="action" value="status">
<button>Status</button></form>`)
		b.WriteString(`</div>`)
	}
	b.WriteString(`</section>`)

	if st.Repo.Exists {
		// --- share card -------------------------------------------------
		b.WriteString(`<section><h2>Invite someone</h2>
<p class="muted">This prints a Port Key. Anyone with it can reach this workspace while the daemon runs.</p>
<div class="grid">
<form method="post" action="/action">
<input type="hidden" name="t" value="` + e(token) + `"><input type="hidden" name="action" value="share">
<input type="hidden" name="lan" value="1">
<label>Port<input name="port" value="` + fmt.Sprint(st.Repo.Port) + `" size="6"></label>
<button>Same Wi-Fi key</button>
<span class="hint">No router setup. Use this when you are on the same network — it is the key that actually connects.</span>
</form>
<form method="post" action="/action">
<input type="hidden" name="t" value="` + e(token) + `"><input type="hidden" name="action" value="share">
<label>Port<input name="port" value="` + fmt.Sprint(st.Repo.Port) + `" size="6"></label>
<button>Public key (UPnP)</button>
<span class="hint">Needs a router that answers UPnP or NAT-PMP. Fails loudly if it does not.</span>
</form>
</div></section>`)

		// --- daemon card ------------------------------------------------
		b.WriteString(`<section><h2>Network</h2>`)
		if st.Daemon.Running {
			b.WriteString(`<p class="live"><span class="dot"></span> daemon running — pid ` + fmt.Sprint(st.Daemon.PID) + `, port ` + fmt.Sprint(st.Daemon.Port) + `</p>`)
		} else {
			b.WriteString(`<p class="muted"><span class="dot off"></span> daemon stopped — nobody can reach this workspace and nothing syncs</p>`)
		}
		b.WriteString(`<div class="row2">`)
		if st.Daemon.Running {
			b.WriteString(`<form method="post" action="/action"><input type="hidden" name="t" value="` + e(token) + `">
<input type="hidden" name="action" value="daemon-stop"><button class="danger">Stop daemon</button></form>`)
		} else {
			b.WriteString(`<form method="post" action="/action"><input type="hidden" name="t" value="` + e(token) + `">
<input type="hidden" name="action" value="daemon-start"><input type="hidden" name="port" value="` + fmt.Sprint(st.Repo.Port) + `">
<button>Start daemon</button></form>`)
		}
		b.WriteString(`<form method="post" action="/action"><input type="hidden" name="t" value="` + e(token) + `">
<input type="hidden" name="action" value="sync"><button>Sync now</button></form></div>`)
		if len(st.Daemon.LogTail) > 0 {
			b.WriteString(`<pre class="log">` + e(strings.Join(st.Daemon.LogTail, "\n")) + `</pre>`)
		}
		b.WriteString(`</section>`)
	}

	// --- updates -------------------------------------------------------
	b.WriteString(`<section><h2>Updates</h2>
<p class="muted">Installs the newest release for ` + e(st.GOOS) + `/` + e(st.GOARCH) + ` after checking it against the release's SHA256SUMS. A download that does not match is thrown away.</p>
<div class="row2">
<form method="post" action="/action"><input type="hidden" name="t" value="` + e(token) + `">
<input type="hidden" name="action" value="update-check"><button>Check</button></form>
<form method="post" action="/action"><input type="hidden" name="t" value="` + e(token) + `">
<input type="hidden" name="action" value="update-apply"><button>Install update</button></form></div>
</section>`)

	// --- doctor --------------------------------------------------------
	b.WriteString(`<section><h2>Checks</h2><dl class="checks">`)
	for _, c := range checks {
		mark := `<span class="bad">✗</span>`
		if c.OK {
			mark = `<span class="good">✓</span>`
		}
		b.WriteString(`<dt>` + mark + ` ` + e(c.Name) + `</dt><dd>` + e(c.Detail) + `</dd>`)
	}
	b.WriteString(`</dl></section>`)

	b.WriteString(`</main><footer>LRM — peer-to-peer workspace sync. This panel answers on 127.0.0.1 only.</footer></body></html>`)
	return []byte(b.String())
}

func row(k, v string) string {
	return `<dt>` + html.EscapeString(k) + `</dt><dd>` + html.EscapeString(v) + `</dd>`
}

const css = `
:root{--bg:#0f1115;--card:#171a21;--line:#262b35;--fg:#e6e9ef;--dim:#98a2b3;--acc:#5ec2a8;--warn:#e8b45c;--bad:#e2686f}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
header{display:flex;align-items:baseline;gap:.6rem;padding:1.1rem 1.6rem;border-bottom:1px solid var(--line)}
header h1{margin:0;font-size:1.15rem;letter-spacing:.14em;text-transform:uppercase}
.pill{border:1px solid var(--line);border-radius:999px;padding:.05rem .5rem;font-size:.75rem;color:var(--dim)}
.sub{color:var(--dim);font-size:.8rem;margin-left:auto}
main{max-width:900px;margin:0 auto;padding:1.4rem 1.2rem 3rem;display:flex;flex-direction:column;gap:1.1rem}
section{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:1.1rem 1.2rem}
section h2{margin:0 0 .7rem;font-size:.78rem;letter-spacing:.13em;text-transform:uppercase;color:var(--dim)}
section h3{margin:0 0 .6rem;font-size:.9rem;font-weight:600}
dl{display:grid;grid-template-columns:9rem 1fr;gap:.35rem .9rem;margin:0}
dt{color:var(--dim)}dd{margin:0;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.84rem;word-break:break-all}
.checks dd{font-family:inherit}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:1rem}
@media(max-width:720px){.grid{grid-template-columns:1fr}}
form{margin:0}
label{display:block;color:var(--dim);font-size:.78rem;margin:.45rem 0}
input{display:block;width:100%;margin-top:.2rem;padding:.45rem .55rem;background:#0c0e12;border:1px solid var(--line);border-radius:6px;color:var(--fg);font:inherit;font-family:ui-monospace,Menlo,monospace}
button{padding:.45rem .85rem;background:var(--acc);color:#06231c;border:0;border-radius:6px;font:inherit;font-weight:600;cursor:pointer}
button:hover{filter:brightness(1.08)}
button.danger{background:transparent;border:1px solid var(--bad);color:var(--bad)}
.row2{display:flex;gap:.6rem;flex-wrap:wrap;margin-top:.4rem;align-items:flex-end}
.inline{display:flex;gap:.5rem;align-items:center}
.inline input{width:auto;margin:0}
.muted{color:var(--dim);margin:.2rem 0 .9rem}
.hint{color:var(--dim);font-size:.76rem;display:block;margin-top:.4rem}
.warn{color:var(--warn)}
.live{color:var(--acc);display:flex;align-items:center;gap:.5rem}
.dot{width:.55rem;height:.55rem;border-radius:50%;background:var(--acc);display:inline-block}
.dot.off{background:var(--dim)}
.good{color:var(--acc)}.bad{color:var(--bad)}
pre{margin:0;padding:.7rem .8rem;background:#0c0e12;border:1px solid var(--line);border-radius:6px;overflow:auto;font:12px/1.45 ui-monospace,Menlo,monospace;white-space:pre-wrap;word-break:break-word}
pre.log{max-height:200px;color:var(--dim)}
.flash{background:#101a16;border:1px solid var(--acc);border-radius:8px;padding:.6rem .8rem}
.flash.err{background:#1b1113;border-color:var(--bad)}
.flash pre{background:transparent;border:0;padding:0}
footer{text-align:center;color:var(--dim);font-size:.75rem;padding:0 1rem 2rem}
`
