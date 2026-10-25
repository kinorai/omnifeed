# ADR 0003: `go fix` modernizers are enforced in CI

- **Status:** proposed (PR #71, not merged). The new-Go-release policy below is
  recommended, not decided.
- **Date:** 2026-10-24
- **Scope:** the Go code style, `make fix` / `make fix-check`, the CI Lint job and the
  pre-commit hooks

## Context

Go 1.26 rebuilt `go fix` around the modernize analyzers. They rewrite old idioms into
current ones without changing behaviour, and they respect the `go` version in
`go.mod`. Examples: `interface{}` to `any`, `for i := 0; i < n; i++` to
`for i := range n`, hand-written map copies to `maps.Copy`, `strings.Split` loops to
`strings.SplitSeq`, `wg.Add(1); go func(){ defer wg.Done() ... }()` to `wg.Go(...)`.

Much of omnifeed is written with coding agents, and the Go team's own blog notes that
LLM assistants keep producing pre-1.22 idioms. On 2026-10-22, `go fix -diff ./...`
proposed rewrites in 22 files. golangci-lint has a `modernize` linter too, but it is not
enabled in `.golangci.yml`.

## Decision

1. **Apply the modernizers once**, in a behaviour-preserving `refactor:` commit.
2. **Enforce them:**
   - `make fix` applies them.
   - `make fix-check` fails when `go fix -diff ./...` prints anything, or exits
     non-zero, so a package that does not build cannot pass as clean. It reads stdout
     only: a `go: downloading ...` line on stderr does not fail a clean tree.
   - The CI Lint job runs `make fix-check` right after `go vet`. A pre-commit hook runs
     the same target, and `make check` includes it.
3. **New Go releases (recommended, open):** each Go release can add rewrites. go1.27's
   `go fix` adds `errors.As` to `errors.AsType`, which go1.26.10 does not propose. The
   gate then passes in CI and fails on a contributor's newer toolchain. The
   recommendation is option (a) below: when a PR bumps `GO_VERSION`, it also runs
   `make fix` and commits the result.

### Options for new Go releases

| Option | For | Against |
|---|---|---|
| (a) Run `make fix` in the PR that bumps `GO_VERSION` (recommended) | One place holds the Go version (`GO_VERSION` in the workflows). The rewrites land with the toolchain that proposes them | A contributor on a newer Go than CI can see `fix-check` fail locally until CI catches up. The fix is to run `make fix`, or to use CI's version |
| (b) Pin `GOTOOLCHAIN` in the Makefile | The check gives the same answer on every machine | The Go version then lives in a second place next to CI's `GO_VERSION`, and the two can drift apart |

A `toolchain` line in `go.mod` does not help: Go never downgrades to an older toolchain
than the one installed, so it cannot hold a newer local Go back to CI's.

For the go1.27 case, PR #71 already applies the `errors.AsType` rewrite. `errors.AsType`
exists in Go 1.26, so the code still builds against `go 1.26.0`, and `fix-check` passes
on both go1.26.10 and go1.27.

## Consequences

- New code cannot drift back to an older idiom without CI saying so. The fix is always
  `make fix`, never a hand edit.
- `make check` stays the single gate: a green `make check` is a green Lint job.
- A modernizer commit is a `refactor:`, so under `cliff.toml` it cuts no release.
- Each `GO_VERSION` bump may carry a `go fix` diff. Review it like any other
  refactor: the analyzers do not change behaviour, but a rename by hand on top of the
  tool's output (as PR #71 did for `before, ok0`) is still a human choice.

## Sources

- Go 1.26 release notes, the `go` command: <https://go.dev/doc/go1.26#go-command>
- "Using go fix to modernize Go code": <https://go.dev/blog/gofix>
