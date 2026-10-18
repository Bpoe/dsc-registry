package migrations

import _ "embed"

//go:embed sqlite/001_initial.sql
var SQLiteInitial string

//go:embed postgres/001_initial.sql
var PostgresInitial string
