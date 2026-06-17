// Package version exposes build-time version metadata.
//
// Version is set by GoReleaser at release time via `-ldflags="-X ..."`. The
// default below applies when running with `go run`, `go build`, or `go test`.
package version

// Version is the semver release string. Stamped by goreleaser.
var Version = "dev"
