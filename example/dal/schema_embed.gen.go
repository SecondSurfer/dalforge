package dal

import (
	"embed"
	"io/fs"
)

// schemaFS embeds all dalforge-generated SQL files at compile time.
//
// Architectural Why: Embedding the SQL files into the compiled Go binary guarantees
// that a Docker image or migration binary tagged e.g. 'v1.2.3' permanently contains
// the exact SQL schema of 'v1.2.3', preventing schema drift from newer files on disk.
//
//go:embed *.sql
var schemaFS embed.FS

// NewSchemaFS returns a read-only filesystem containing the embedded SQL definitions.
func NewSchemaFS() fs.FS {
	return schemaFS
}
