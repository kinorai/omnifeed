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
