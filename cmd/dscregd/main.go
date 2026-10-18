package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/httpapi"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	settings, err := config.Parse(os.Args[1:], os.Stdout)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, settings, logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, settings config.Settings, logger *slog.Logger) (err error) {
	if err := settings.Validate(); err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, settings.StartupTimeout)
	defer cancel()
	reports, err := reportstore.Open(startup, settings.ReportRoot)
	if err != nil {
		return fmt.Errorf("validate report storage: %w", err)
	}
	defer func() { err = errors.Join(err, reports.Close()) }()
	db, err := database.Open(startup, settings.DatabaseDSN, settings.SQLiteBusyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", settings.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	server := &http.Server{
		Handler:           httpapi.New(db, reports, settings.ProbeTimeout, logger),
		ReadHeaderTimeout: settings.ReadHeaderTimeout,
		ReadTimeout:       settings.ReadTimeout,
		WriteTimeout:      settings.WriteTimeout,
		IdleTimeout:       settings.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(&serverErrorLog{logger: logger}, "", 0),
	}
	logger.Info("server listening", "address", listener.Addr().String())
	return serve(ctx, server, listener, settings, logger)
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, settings config.Settings, logger *slog.Logger) (err error) {
	defer func() { err = errors.Join(err, server.Close()) }()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("server shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), settings.ShutdownTimeout)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			err = errors.Join(err, server.Close())
		}
		serveErr := <-done
		if !errors.Is(serveErr, http.ErrServerClosed) {
			err = errors.Join(err, serveErr)
		}
		return err
	}
}

type serverErrorLog struct {
	logger *slog.Logger
}

func (w *serverErrorLog) Write(p []byte) (int, error) {
	// net/http diagnostics can contain untrusted request data or panic values.
	w.logger.Error("HTTP server transport or handler error")
	return len(p), nil
}
