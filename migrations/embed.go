// Package migrations contains the authoritative database schema for every deployment.
package migrations

import "embed"

// Files travels with the binaries, so workers and tests never need an inline
// approximation of the schema when launched outside the repository directory.
//
//go:embed *.sql
var Files embed.FS
