package service

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	. "github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/migrations"
	"github.com/google/uuid"
)

// The adapter preserves the existing workflow fault-injection tests while
// exercising service orchestration and concrete database transactions.
type reportTestStore struct{ *database.Store }

func wrapStore(db *database.Store) *reportTestStore { return &reportTestStore{db} }

type callbackFiles struct {
	write func() (string, error)
	read  func(string) ([]byte, error)
}

func (f callbackFiles) Write(context.Context, []byte) (string, error) { return f.write() }
func (f callbackFiles) Read(_ context.Context, path string, _ int64) ([]byte, error) {
	return f.read(path)
}
func (callbackFiles) Remove(string) error { panic("unexpected cleanup in workflow adapter") }
func testService(db *database.Store, files reportFiles) *Service {
	return &Service{db: db, files: files, maxReportBytes: 1 << 20, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}
func (s *reportTestStore) PublishReport(ctx context.Context, id, name string, upload report.Upload, write func() (string, error), read func(string) ([]byte, error)) (database.Publication, error) {
	return testService(s.Store, callbackFiles{write: write, read: read}).publishReport(ctx, id, name, upload)
}
func (s *reportTestStore) ReadReport(ctx context.Context, id, name string, read func(string) error) error {
	_, err := testService(s.Store, callbackFiles{read: func(path string) ([]byte, error) { return nil, read(path) }}).ReadReport(ctx, id, name)
	return err
}
func (s *reportTestStore) GetReport(ctx context.Context, id, reportID string, read func(string) error) (database.ReportMetadata, error) {
	item, _, err := testService(s.Store, callbackFiles{read: func(path string) ([]byte, error) { return nil, read(path) }}).GetReport(ctx, id, reportID, read != nil)
	return item, err
}
func (s *reportTestStore) RetireReport(ctx context.Context, path string, remove func(string) error) error {
	var referenced bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM reports WHERE report_path = $1)`, path).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return nil
	}
	return remove(path)
}

// The legacy migration fixture contains exactly one machine named host.
func backfillHostName(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, "UPDATE machines SET name_folded = 'host'")
	return err
}
func testStore(t *testing.T, engine, dsn string) *reportTestStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := Connect(ctx, engine, dsn, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	return wrapStore(store)
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func assertCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
		t.Fatalf("%s count=%d, want=%d, error=%v", table, count, want, err)
	}
}

func parseDesired(t *testing.T, body string) desired.Content {
	t.Helper()
	content, err := desired.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func validRevision(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func machineNamesOldDatabase(t *testing.T, engine, dsn string) (*sql.DB, int) {
	t.Helper()
	driver, initial, version := "sqlite", migrations.SQLiteInitial, 1
	if engine == "postgres" {
		driver, initial, version = "pgx", migrations.PostgresInitial, 2
	}
	raw, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	exec(t, raw, initial)
	if engine == "postgres" {
		exec(t, raw, migrations.PostgresLosslessJSON)
	}
	exec(t, raw, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)")
	for v := 1; v <= version; v++ {
		exec(t, raw, "INSERT INTO schema_migrations VALUES ($1, 'original')", v)
	}
	return raw, version
}

func (s *reportTestStore) GetMachineStatus(ctx context.Context, id string) (MachineStatus, error) {
	return (&Service{db: s.Store}).GetMachineStatus(ctx, id)
}
func (s *reportTestStore) GetConfigurationStatus(ctx context.Context, id, name string) (ConfigurationStatus, error) {
	return (&Service{db: s.Store}).GetConfigurationStatus(ctx, id, name)
}
