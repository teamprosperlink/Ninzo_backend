// Package migrations holds the database schema. Applied files are never edited: a
// change is a new file.
package migrations

import "embed"

// FS holds the SQL files.
//
//go:embed *.sql
var FS embed.FS
