// Package contracts gives Go code access to the bundled API contracts.
package contracts

import "embed"

// Dist holds the single-file contracts.
//
//go:embed dist/*.yaml
var Dist embed.FS
