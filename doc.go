// Package colony resolves contributor affiliation and activity from public
// CNCF sources (cncf/gitdm, CNCF devstats, LFX Insights).
//
// The library is consumer-neutral: it imports nothing from any consumer and
// sends no identity of its own. See README.md for the privacy model of each
// provider.
package colony

// Version is the library version. Release builds override it with
// -ldflags "-X github.com/hivecommons/colony.Version=vX.Y.Z"; the shared HTTP
// client uses it in its generic User-Agent ("colony/<version>").
var Version = "dev"
