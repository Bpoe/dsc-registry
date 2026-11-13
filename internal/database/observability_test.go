package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/Bpoe/dsc-registry/internal/testotel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestDatabaseTelemetryBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		capture := testotel.Setup(t)
		store := testStore(t, engine, dsn)
		ctx, parent := otel.Tracer("test").Start(context.Background(), "originating service")
		var got string
		if err := store.DB.QueryRowContext(ctx, "SELECT $1", "private-parameter").Scan(&got); err != nil || got != "private-parameter" {
			t.Fatal(err)
		}
		_, _ = store.DB.ExecContext(ctx, "SELECT private-query-text")
		// Exercise database/sql's pool wait path without requiring a slow query.
		store.DB.SetMaxOpenConns(1)
		conn, err := store.DB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		_, err = store.DB.Conn(waitCtx)
		cancel()
		if err == nil {
			t.Fatal("pool contention not exercised")
		}
		_ = conn.Close()
		parent.End()
		found, failure := false, false
		for _, span := range capture.Spans.Ended() {
			if span.InstrumentationScope().Name != "github.com/XSAM/otelsql" {
				continue
			}
			if span.Parent().SpanID() == parent.SpanContext().SpanID() {
				found = true
			}
			for _, a := range span.Attributes() {
				if a.Key == "error.type" {
					failure = true
				}
				if a.Key == "db.query.text" || strings.Contains(a.Value.Emit(), "private-") || a.Value.Emit() == dsn {
					t.Fatal("private SQL telemetry", a)
				}
			}
			if len(span.Events()) != 0 {
				t.Fatal("raw driver error events enabled")
			}
		}
		if !found || !failure {
			t.Fatalf("SQL child span=%v, safe error attribute=%v", found, failure)
		}
		metrics := capture.Collect(t)
		if _, ok := metrics["db.client.operation.duration"].Data.(metricdata.Histogram[float64]); !ok {
			t.Fatal("missing standard SQL duration")
		}
		for _, name := range []string{"db.sql.connection.max_open", "db.sql.connection.open", "db.sql.connection.wait", "db.sql.connection.wait_duration", "db.sql.connection.closed_max_idle", "db.sql.connection.closed_max_idle_time", "db.sql.connection.closed_max_lifetime"} {
			if _, ok := metrics[name]; !ok {
				t.Fatal("missing pool metric", name)
			}
		}
		waits := metrics["db.sql.connection.wait"].Data.(metricdata.Sum[int64])
		if len(waits.DataPoints) != 1 || waits.DataPoints[0].Value < 1 {
			t.Fatal("pool wait not observed")
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		for _, scope := range capture.Collect(t) {
			if strings.HasPrefix(scope.Name, "db.sql.connection.") {
				t.Fatal("pool observer still registered after Close", scope.Name)
			}
		}
	})
}
