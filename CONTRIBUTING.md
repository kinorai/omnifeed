# Contributing

## Quick start

```bash
git clone https://github.com/kinorai/omnifeed.git
cd omnifeed
make install-tools        # golangci-lint, govulncheck, gomarkdoc
make pre-commit-install   # git hooks that mirror CI, once per clone
make check                # vet + lint + test, the targets CI runs
```

CI and the pre-commit hooks call the same `make` targets, so a green `make check`
or `make pre-commit-run` means a green CI lint job.

## Conventional Commits

Use [Conventional Commits](https://www.conventionalcommits.org/):

- `feat:` new functionality
- `fix:` bug fix
- `refactor:` restructuring without behavior change
- `docs:` documentation only
- `test:` tests only
- `chore:` tooling, deps, build, CI
- `perf:` performance improvement

git-cliff and goreleaser read the history to set the version and changelog. `feat`
cuts a minor release. `fix`, `perf`, `chore(deps)` and `chore(docker)` cut a patch
release that rebuilds and publishes the image. Other types don't release.

## Pull request checklist

- [ ] `make check` passes, or `make pre-commit-run` for full CI parity
- [ ] New behavior has a test
- [ ] Commit messages follow Conventional Commits
- [ ] README and docs updated if user-visible behavior changed

## Adding an engine

1. Create `internal/engine/<name>/engine.go` implementing `domain.Engine`.
2. Register it in `cmd/omnifeed/main.go` with `registry.Register(...)` before the fallback.
3. Add a `*_test.go` covering URL matching and at least one fixture.
4. Add it to the README's *Architecture* section and `fetch_url` bullet. Put new env vars in the `docs/configuration.md` table.

## Adding a transport

1. Create `internal/transport/<name>/server.go` taking `*engine.Registry`.
2. Mount it from `main.go` on its own listener.
3. Document the endpoint shape in the README.

## Reporting bugs

Open an issue with:

- The version, from `docker image ls | grep omnifeed` or the git SHA
- The URL or request that triggered it
- The full request and response, secrets redacted
- Relevant logs, which are structured JSON

## License

Contributions are released under the [MIT License](LICENSE).
