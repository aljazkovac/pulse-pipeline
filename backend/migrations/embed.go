// Package migrations embeds the golang-migrate SQL files so every binary and
// test applies exactly the same schema (ADR-0002 §6).
package migrations

import "embed"

// FS holds the NNNNNN_name.{up,down}.sql migration files.
//
//go:embed *.sql
var FS embed.FS
