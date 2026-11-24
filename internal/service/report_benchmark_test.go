package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

func BenchmarkReportReplacement(b *testing.B) {
	for _, engine := range []string{"sqlite", "postgres"} {
		for _, machines := range []int{1, 64} {
			b.Run(fmt.Sprintf("%s/machines=%d", engine, machines), func(b *testing.B) {
				benchmarkReportReplacement(b, engine, machines)
			})
		}
	}
}

func benchmarkReportReplacement(b *testing.B, engine string, machines int) {
	dsn := filepath.Join(b.TempDir(), "registry.db")
	if engine == "postgres" {
		dsn = testdb.Postgres(b)
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, engine, dsn, 5*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	files, err := reportstore.Open(ctx, b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { files.Close() })
	app := New(db, files, nil, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	desiredContent, err := desired.Parse([]byte(`{"template":{}}`))
	if err != nil {
		b.Fatal(err)
	}
	type target struct{ id, revision string }
	targets := make([]target, 0, machines)
	for i := range machines {
		machine, err := db.CreateMachine(ctx, fmt.Sprintf("benchmark-%03d", i))
		if err != nil {
			b.Fatal(err)
		}
		configuration, _, err := db.PutConfiguration(ctx, machine.ID, "one", desiredContent)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := app.PublishReport(ctx, machine.ID, serviceReportBody(configuration.Revision, uuid.NewString())); err != nil {
			b.Fatal(err)
		}
		targets = append(targets, target{machine.ID, configuration.Revision})
	}
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.RunReportCleanup(workerCtx)
	}()
	b.Cleanup(func() {
		stop()
		<-done
	})

	var uploadIndex atomic.Uint64
	before := db.DB.Stats()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			target := targets[uploadIndex.Add(1)%uint64(len(targets))]
			result, err := app.PublishReport(ctx, target.id, serviceReportBody(target.revision, uuid.NewString()))
			if err != nil || result.Outcome != database.MutationUpdated {
				b.Errorf("replacement failed: %v, %v", result, err)
				return
			}
		}
	})
	b.StopTimer()
	after := db.DB.Stats()
	b.ReportMetric(float64(after.WaitCount-before.WaitCount)/float64(b.N), "pool-waits/op")
	b.ReportMetric(float64(after.WaitDuration-before.WaitDuration)/float64(b.N), "pool-wait-ns/op")
}
