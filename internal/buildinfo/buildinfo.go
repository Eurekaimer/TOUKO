// Package buildinfo contains release metadata injected with go build -ldflags.
package buildinfo

// Development builds deliberately do not claim a release version or commit.
var (
	Version = "dev"
	Commit  = "unknown"
)

func String() string { return Version + " (commit " + Commit + ")" }
