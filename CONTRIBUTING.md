# Contributing

Thanks for considering a contribution!

## Quick start

```bash
git clone https://github.com/kinorai/omnifeed.git
cd omnifeed
make install-tools        # golangci-lint, govulncheck, gomarkdoc
make pre-commit-install   # git hooks that mirror CI (run once per clone)
make check                # vet + lint + test — the same targets CI runs
```

The Makefile is the single source of truth for "clean repo": CI and the
pre-commit hooks call the same `make` targets, so a green `make check` (or
`make pre-commit-run`) locally means a green CI lint job.

## Conventional Commits

This repo follows [Conventional Commits](https://www.conventionalcommits.org/).
Commit prefixes:

- `feat:` — new functionality
- `fix:` — bug fix
- `refactor:` — code restructuring without behavior change
- `docs:` — documentation only
- `test:` — tests only
- `chore:` — tooling, deps, build, CI
- `perf:` — performance improvement

The release workflow (git-cliff + goreleaser) reads commit history to version
and changelog automatically: `feat` cuts a minor release, `fix`/`perf` and
dependency bumps (`chore(deps)` / `chore(docker)`) cut a patch release that
rebuilds and publishes the image; other types don't release.

## Pull request checklist

- [ ] `make check` passes locally (vet + lint + test) — or `make pre-commit-run` for full CI parity
- [ ] New behaviour has a test
- [ ] Commit messages follow Conventional Commits
- [ ] README/docs updated if user-visible behaviour changed

## Adding a new engine (e.g. Hacker News)

1. Create `internal/engine/<name>/engine.go` implementing `domain.Engine`.
2. Wire it in `cmd/omnifeed/main.go` via `registry.Register(...)` **before** the fallback.
3. Add a `*_test.go` covering URL matching + at least one fixture.
4. Document it in README under *Architecture* and in the supported-sources list.

## Adding a new transport (e.g. OpenAI tool-call endpoint)

1. Create `internal/transport/<name>/server.go` that takes `*engine.Registry`.
2. Mount it from `main.go` on its own listener.
3. Document the endpoint shape in README.

## Reporting bugs

Open an issue with:

- Version (`docker image ls | grep omnifeed` or git SHA)
- The URL or request that triggered the issue
- Full request and response (redact secrets)
- Relevant logs (the proxy emits structured JSON)

## License

By contributing, you agree your work is released under the [MIT License](LICENSE).
