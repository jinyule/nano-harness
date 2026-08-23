// Package version exposes build identity without coupling runtime packages to
// the command entry point.
package version

import "fmt"

var (
	// Version is the semantic release version injected at build time.
	Version = "dev"
	// Commit is the source revision injected at build time.
	Commit = "unknown"
	// Date is the UTC build timestamp injected at build time.
	Date = "unknown"
)

// Info identifies one nano-harness build.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Current returns the identity injected into the running binary.
func Current() Info {
	return Info{Version: Version, Commit: Commit, Date: Date}
}

// String returns a stable, human-readable build identity.
func (info Info) String() string {
	if info.Version == "dev" {
		return "nano-harness dev"
	}
	return fmt.Sprintf("nano-harness %s (commit %s, built %s)", info.Version, info.Commit, info.Date)
}
