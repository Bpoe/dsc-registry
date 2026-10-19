package migrations

import _ "embed"

//go:embed sqlite/001_initial.sql
var SQLiteInitial string

//go:embed sqlite/002_report_history.sql
var SQLiteReportHistory string

//go:embed postgres/001_initial.sql
var PostgresInitial string

//go:embed postgres/002_lossless_json.sql
var PostgresLosslessJSON string

//go:embed postgres/003_report_history.sql
var PostgresReportHistory string
