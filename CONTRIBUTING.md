# Contributing

Thanks for considering a contribution!

## Quick start

```bash
git clone https://github.com/kinorai/crawl4ai-reddit-proxy.git
cd crawl4ai-reddit-proxy
go test ./...
go build ./...
```

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

The release workflow uses commit history to generate the changelog automatically.

## Pull request checklist

- [ ] `go test ./...` passes locally
- [ ] `go vet ./...` clean
- [ ] `golangci-lint run` clean (or document why)
- [ ] New behaviour has a test
- [ ] Commit messages follow Conventional Commits
- [ ] README/docs updated if user-visible behaviour changed

## Adding a new engine (e.g. Hacker News)

1. Create `internal/engine/<name>/engine.go` implementing `domain.Engine`.
2. Wire it in `cmd/crawl4ai-reddit-proxy/main.go` via `registry.Register(...)` **before** the fallback.
3. Add a `*_test.go` covering URL matching + at least one fixture.
4. Document it in README under *Architecture* and in the supported-sources list.

## Adding a new transport (e.g. OpenAI tool-call endpoint)

1. Create `internal/transport/<name>/server.go` that takes `*engine.Registry`.
2. Mount it from `main.go` on its own listener.
3. Document the endpoint shape in README.

## Reporting bugs

Open an issue with:

- Version (`docker image ls | grep crawl4ai-reddit-proxy` or git SHA)
- The URL or request that triggered the issue
- Full request and response (redact secrets)
- Relevant logs (the proxy emits structured JSON)

## License

By contributing, you agree your work is released under the [MIT License](LICENSE).
