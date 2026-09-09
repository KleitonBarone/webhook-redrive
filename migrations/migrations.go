package migrations

import "embed"

// Files contains the ordered SQL migrations applied at API startup and in tests.
//
//go:embed *.sql
var Files embed.FS
