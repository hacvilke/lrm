## What this changes

<!-- Why, not what. The diff already says what. -->

## Checklist

- [ ] `make test` passes
- [ ] `scripts/check-all.sh ./lrm` is green (90 checks)
- [ ] `gofmt` and `go vet` are clean
- [ ] **No new third-party dependencies** (`go.mod` still has no `require` block)
- [ ] No hard-coded `/usr/local`, `/etc` or `/var` paths
- [ ] Desktop-only: no Android-specific code (that belongs in lrm-mobile)

## If this touches path handling, the dashboard, or the `.lr` runtime

- [ ] Existing security tests still pass
- [ ] I extended them to cover the new surface

<!--
There are tests that feed ../../pwned and /etc/passwd into the tree layer
and assert rejection, and tests that the dashboard cannot escape the repo
root or read identity.key. Keep them meaningful.
-->
