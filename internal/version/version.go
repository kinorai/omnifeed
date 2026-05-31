// Package version exposes build-time version metadata.
//
// Version, Commit, and Date are set by GoReleaser at release time via
// `-ldflags="-X ..."`. The defaults below apply when running with `go run`,
// `go build`, or `go test`.
package version

var (
	// Version is the semver release string. Stamped by goreleaser.
	Version = "dev"
	// Commit is the full git SHA at build time. Stamped by goreleaser.
	Commit = "none"
	// Date is the build timestamp (RFC3339). Stamped by goreleaser.
	Date = "unknown"
)

// UserAgentTemplate produces a Reddit-compliant User-Agent following the
// format `<platform>:<app-id>:<version> (+<url>)` recommended by Reddit's
// API rules. A unique, identifiable UA earns more generous rate limits
// than browser-impersonating UAs.
func UserAgentTemplate() string {
	return "go:crawl4ai-reddit-proxy:v" + Version + " (+https://github.com/kinorai/crawl4ai-reddit-proxy)"
}
