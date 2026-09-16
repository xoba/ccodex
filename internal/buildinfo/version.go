// Package buildinfo identifies the ccodex build for CLI output and the Codex protocol.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Version is set by release builds with -ldflags -X. Source checkouts use dev;
// go install builds can recover their version from Go's module metadata.
var Version = "dev"

func init() {
	if Version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = strings.TrimPrefix(info.Main.Version, "v")
	}
}
