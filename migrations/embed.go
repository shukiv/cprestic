// Package migrations holds the SQL schema and embeds it for the migration
// runner in internal/store.
package migrations

import "embed"

// FS contains every .sql migration, applied in filename order.
//
//go:embed *.sql
var FS embed.FS
