// Package colony resolves contributor affiliation and activity from public
// CNCF sources (cncf/gitdm, CNCF devstats, LFX Insights).
//
// The library is consumer-neutral: it imports nothing from any consumer and
// sends no identity of its own. See README.md for the privacy model of each
// provider.
package colony

import "runtime/debug"

// ModulePath is this module's import path.
const ModulePath = "github.com/hivecommons/colony"

const develVersion = "(devel)"

// Version is the library version. Release builds may set it with
// -ldflags "-X github.com/hivecommons/colony.Version=vX.Y.Z"; otherwise it is
// read from the build info (the colony module version the binary was built
// with). The shared HTTP client uses it in its generic User-Agent
// ("colony/<version>").
var Version = "dev"

func init() {
	if Version != "dev" {
		return
	}
	if v := versionFromBuildInfo(); v != "" {
		Version = v
	}
}

func versionFromBuildInfo() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if bi.Main.Path == ModulePath && bi.Main.Version != "" && bi.Main.Version != develVersion {
		return bi.Main.Version
	}
	for _, dep := range bi.Deps {
		if dep.Path == ModulePath && dep.Version != "" && dep.Version != develVersion {
			return dep.Version
		}
	}
	return ""
}
