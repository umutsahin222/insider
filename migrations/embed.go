// Package migrations embeds the versioned SQL schema so tests and tools can apply it
// without depending on the working directory.
package migrations

import "embed"

// FS holds every *.sql migration in golang-migrate naming (NNNNNN_name.{up,down}.sql).
//
//go:embed *.sql
var FS embed.FS
