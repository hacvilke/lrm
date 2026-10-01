# Contributing to LRM

Thanks for considering it. LRM is a peer-to-peer version control system
with **zero dependencies**, and most of the rules below exist to keep that
true.

## Getting set up

```sh
git clone https://github.com/hacvilke/lrm
cd lrm
make build                      # produces ./lrm
make test                       # unit tests
scripts/check-all.sh ./lrm      # full CLI surface sweep (90 checks)
```

`check-all.sh` exercises the entire command surface against real
repositories and a live daemon. It should be green before and after your
change. `KEEP=1` keeps the scratch directory if you need to poke at it.

## How to propose a change

`main` is protected. **Nobody pushes to it directly — including the
maintainers.** Everything goes through a pull request with passing CI.

```sh
git checkout -b fix/some-thing
# ... work ...
make test && scripts/check-all.sh ./lrm
git commit
git push -u origin fix/some-thing
gh pr create          # or open the PR in the web UI
```

### Commit messages

Explain *why*, not *what* — the diff already says what. State the
observed behaviour, the cause, and what the change deliberately does not
attempt.

## Standards

- **No third-party dependencies.** `go.mod` has no `require` block and
  that is a feature, not an accident: it is why this codebase can be
  audited and why it cross-compiles everywhere. A PR that adds a
  dependency needs to justify itself in an issue first.
- **`gofmt` clean, `go vet` clean, `make test` green,
  `scripts/check-all.sh` green.**
- **Keep the paths portable.** State resolves through `$LRM_HOME`, then
  `os.UserHomeDir()`, then `~/.lrm`. Do not hard-code `/usr/local`,
  `/etc` or `/var`; those break on Android, on immutable distributions,
  and for unprivileged users.
- **Security tests are not optional.** There are tests that feed
  `../../pwned` and `/etc/passwd` into the tree layer and assert
  rejection, and tests that the dashboard cannot escape the repo root or
  read `identity.key`. If you touch path handling or the dashboard,
  extend them.
- **Split large files rather than growing them.** `internal/cli/cli.go`
  and `internal/daemon/daemon.go` are already too long; new surface
  should land in a new file.

## Platform support

This repository targets **desktop only**: Linux, macOS and Windows, on
amd64 and arm64.

**Android and Termux are handled by a separate project**,
[lrm-mobile](https://github.com/hacvilke/lrm-mobile), which consumes this
repository unchanged as a git submodule and adds the Android build,
installer and platform detection around it.

Please do not add Android-specific code here. A desktop `linux/arm64`
binary cannot even start on Android — Go's default build is `ET_EXEC` and
Android's loader requires a position-independent executable — so the fix
belongs in the build layer, not in the engine. If the engine genuinely
needs a change for mobile, it must be **platform-aware rather than
Android-only**, and it should come with a reason why it cannot live in
lrm-mobile.

The installer here deliberately refuses to run on Android and redirects to
lrm-mobile. Keep that behaviour.

## Reporting bugs

Open an issue using the bug template. Include `lrm version`, your OS and
CPU, and the exact commands you ran. For sync or mesh problems, the output
of `lrm status` and `lrm replog --last 20` on both ends is usually what
settles it.

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).

## Licensing of contributions

This project is licensed under the [Apache License 2.0](LICENSE). Under
section 5 of that licence, any contribution you intentionally submit for
inclusion is licensed under the same terms, with no separate paperwork —
there is no CLA to sign.

Only submit work you have the right to license that way.
