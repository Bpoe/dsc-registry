package service

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/Bpoe/dsc-registry/internal/testotel"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestReportTelemetryBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		capture := testotel.Setup(t)
		db, files, _, machine, config := reportSetup(t, engine, dsn)
		app := New(db.Store, files, nil, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ctx, parent := otel.Tracer("test").Start(context.Background(), "upload request")
		first := serviceReportBody(config.Revision, uuid.NewString())
		updated := serviceReportBody(config.Revision, uuid.NewString())
		conflict := []byte(strings.Replace(string(updated), `"opaque":9007199254740993123456789`, `"opaque":"private-report"`, 1))
		bodies := [][]byte{first, first, updated, conflict, []byte(`{}`)}
		expected := []database.MutationOutcome{database.MutationCreated, database.MutationUnchanged, database.MutationUpdated}
		var sizeSum int64
		for i, body := range bodies {
			result, err := app.PublishReport(ctx, machine.ID, body)
			if i < 3 {
				if err != nil || result.Outcome != expected[i] {
					t.Fatalf("attempt %d: %v, %v", i, result, err)
				}
			} else if err == nil {
				t.Fatal("invalid ingestion accepted")
			}
			if upload, err := report.Parse(body); err == nil {
				sizeSum += int64(len(upload.Result))
			}
		}
		// A parsed attempt failing storage still contributes size, publication lookup,
		// and exactly one failed ingestion. SQL failure likewise remains an attempt.
		_ = files.Close()
		body := serviceReportBody(config.Revision, uuid.NewString())
		if _, err := app.PublishReport(ctx, machine.ID, body); err == nil {
			t.Fatal("storage failure missing")
		}
		sizeSum += int64(len(serviceResult))
		_ = db.Close()
		if _, err := app.PublishReport(ctx, machine.ID, body); err == nil {
			t.Fatal("database failure missing")
		}
		sizeSum += int64(len(serviceResult))
		parent.End()
		metrics := capture.Collect(t)
		histogram := metrics["dscreg.report.ingest.duration"].Data.(metricdata.Histogram[float64])
		outcomes := map[string]uint64{}
		for _, dp := range histogram.DataPoints {
			if dp.Attributes.Len() != 1 {
				t.Fatal("unexpected ingestion dimensions")
			}
			outcome, _ := dp.Attributes.Value("dscreg.report.ingest.outcome")
			outcomes[outcome.AsString()] += dp.Count
		}
		if outcomes["created"] != 2 || outcomes["duplicate"] != 1 || outcomes["failed"] != 4 || len(outcomes) != 3 {
			t.Fatal("ingestion classification/count", outcomes)
		}
		size := metrics["dscreg.report.size"].Data.(metricdata.Histogram[int64])
		if len(size.DataPoints) != 1 || size.DataPoints[0].Count != 6 || size.DataPoints[0].Sum != sizeSum || size.DataPoints[0].Attributes.Len() != 0 {
			t.Fatal("report envelope size/count incorrect", size)
		}
		publish := metrics["dscreg.report.publish.duration"].Data.(metricdata.Histogram[float64])
		if len(publish.DataPoints) != 1 || publish.DataPoints[0].Count != 6 || publish.DataPoints[0].Attributes.Len() != 0 {
			t.Fatal("publication must be once per parsed attempt")
		}
		custom := 0
		for name := range metrics {
			if strings.HasPrefix(name, "dscreg.") {
				custom++
			}
		}
		if custom != 4 {
			t.Fatalf("custom metric count = %d; expected four histograms", custom)
		}
		serviceSpans, storageSpans, sqlChildren, failed := 0, 0, 0, 0
		for _, span := range capture.Spans.Ended() {
			if span.Name() == "Service.PublishReport" {
				serviceSpans++
				if span.Parent().SpanID() != parent.SpanContext().SpanID() {
					t.Fatal("service context lost")
				}
				if span.Status().Code == codes.Error {
					failed++
				}
			}
			if span.Name() == "Report storage write" {
				storageSpans++
			}
			if span.InstrumentationScope().Name == "github.com/XSAM/otelsql" && span.SpanContext().TraceID() == parent.SpanContext().TraceID() {
				sqlChildren++
			}
		}
		if serviceSpans != 7 || storageSpans != 3 || sqlChildren == 0 || failed != 4 {
			t.Fatalf("service=%d storage=%d sql=%d failed=%d", serviceSpans, storageSpans, sqlChildren, failed)
		}
	})
}

// delayFiles keeps the production I/O behavior while making phase boundaries
// observable without relying on database query speed.
type delayFiles struct {
	*reportstore.Store
	delay time.Duration
}

func (f delayFiles) Write(ctx context.Context, body []byte) (string, error) {
	time.Sleep(f.delay)
	return f.Store.Write(ctx, body)
}

func TestPublicationTimingExcludesFilesystem(t *testing.T) {
	capture := testotel.Setup(t)
	db, files, _, machine, config := reportSetup(t, "sqlite", filepath.Join(t.TempDir(), "db"))
	app := New(db.Store, files, nil, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app.files = delayFiles{Store: files, delay: 25 * time.Millisecond}
	if _, err := app.PublishReport(context.Background(), machine.ID, serviceReportBody(config.Revision, uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	metrics := capture.Collect(t)
	ingest := metrics["dscreg.report.ingest.duration"].Data.(metricdata.Histogram[float64]).DataPoints[0]
	publication := metrics["dscreg.report.publish.duration"].Data.(metricdata.Histogram[float64]).DataPoints[0]
	if ingest.Sum-publication.Sum < .025 {
		t.Fatal("publication included filesystem delay")
	}
}

type retiringFiles struct {
	*reportstore.Store
	beforeRead func()
}

func (f retiringFiles) Read(ctx context.Context, key string, limit int64) ([]byte, error) {
	f.beforeRead()
	return f.Store.Read(ctx, key, limit)
}

func TestPublicationRetryRecordsOneLogicalMeasurement(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		capture := testotel.Setup(t)
		db, files, _, machine, config := reportSetup(t, engine, dsn)
		ctx := context.Background()
		body := serviceReportBody(config.Revision, uuid.NewString())
		upload, err := report.Parse(body)
		if err != nil {
			t.Fatal(err)
		}
		// Publish the migrated-record fixture directly, so setup contributes no
		// service-level ingestion/publication measurements.
		key, err := files.Write(ctx, upload.Result)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(upload.Result)
		if _, _, err := db.CommitReport(ctx, machine.ID, upload.Name, upload, key, digest[:], ""); err != nil {
			t.Fatal(err)
		}
		exec(t, db.DB, "UPDATE reports SET result_sha256 = NULL WHERE machine_id = $1", machine.ID)
		app := New(db.Store, files, nil, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
		app.files = retiringFiles{Store: files, beforeRead: func() {
			replacement := upload
			replacement.ID = uuid.NewString()
			next, err := files.Write(ctx, replacement.Result)
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := db.CommitReport(ctx, machine.ID, replacement.Name, replacement, next, digest[:], "")
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range result.CleanupPaths {
				if err := files.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
		}}
		if result, err := app.PublishReport(ctx, machine.ID, body); err != nil || result.Outcome != database.MutationUpdated {
			t.Fatalf("retry failed: %+v, %v", result, err)
		}
		metrics := capture.Collect(t)
		for _, name := range []string{"dscreg.report.ingest.duration", "dscreg.report.publish.duration"} {
			histogram := metrics[name].Data.(metricdata.Histogram[float64])
			if len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != 1 {
				t.Fatal("retry recorded multiple logical attempts", name, histogram)
			}
		}
		retries := 0
		for _, span := range capture.Spans.Ended() {
			if span.Name() == "Service.PublishReport" {
				for _, event := range span.Events() {
					if event.Name == "Report publication retry" {
						retries++
					}
				}
			}
		}
		if retries != 1 {
			t.Fatalf("retry events = %d", retries)
		}
	})
}
