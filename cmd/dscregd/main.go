package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/httpapi"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/service"
	"github.com/Bpoe/dsc-registry/internal/telemetry"
)

func main() {
	if printVersion(os.Args[1:], os.Stdout) {
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		if err := bootstrap(context.Background(), os.Args[2:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
			logger.Error("bootstrap failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if handled, err := platformService(os.Args[1:]); handled {
		if err != nil {
			logger.Error("Windows service failed", "reason", "service_dispatch_failure")
			os.Exit(1)
		}
		return
	}
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
		// OS and driver errors can contain protected paths or connection data.
		logger.Error("server stopped", "reason", "startup_or_shutdown_failure")
		os.Exit(1)
	}
}

func run(ctx context.Context, settings config.Settings, logger *slog.Logger) (err error) {
	return runWithReady(ctx, settings, logger, nil)
}

func runWithReady(ctx context.Context, settings config.Settings, logger *slog.Logger, ready func()) (err error) {
	if err := settings.Validate(); err != nil {
		return err
	}
	serverLogger, shutdownTelemetry, err := telemetry.Init(ctx, version, logger)
	if err != nil {
		logger.Error("telemetry initialization failed", "error", err)
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdown); err != nil {
			logger.Warn("telemetry shutdown incomplete", "reason", "flush_failed")
		}
	}()
	var tlsConfig *tls.Config
	if settings.TLS != nil {
		certificate, err := tls.LoadX509KeyPair(settings.TLS.CertFile, settings.TLS.KeyFile)
		if err != nil {
			serverLogger.Error("TLS initialization failed", "reason", "certificate_key_pair_unreadable_or_invalid")
			return errors.New("cannot load TLS certificate and private key; require readable PEM files with a matching key")
		}
		tlsConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
		}
	}
	startup, cancel := context.WithTimeout(ctx, settings.StartupTimeout)
	defer cancel()
	reports, err := reportstore.Open(startup, settings.ReportRoot)
	if err != nil {
		serverLogger.Error("startup dependency failed", "dependency", "report_storage")
		return fmt.Errorf("validate report storage: %w", err)
	}
	defer func() { err = errors.Join(err, reports.Close()) }()
	db, err := database.Connect(startup, settings.DatabaseEngine, settings.DatabaseDSN, settings.SQLiteBusyTimeout)
	if err != nil {
		serverLogger.Error("startup dependency failed", "dependency", "database")
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.Logger = serverLogger
	var publisher *eventing.AMQP
	if settings.Eventing != nil {
		publisher, err = eventing.NewAMQP(*settings.Eventing, serverLogger)
		if err != nil {
			return err
		}
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", settings.ListenAddress)
	if err != nil {
		serverLogger.Error("HTTP listener startup failed")
		return fmt.Errorf("listen HTTP: %w", err)
	}
	server := &http.Server{
		ReadHeaderTimeout: settings.ReadHeaderTimeout,
		ReadTimeout:       settings.ReadTimeout,
		WriteTimeout:      settings.WriteTimeout,
		IdleTimeout:       settings.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(&serverErrorLog{logger: serverLogger}, "", 0),
		TLSConfig:         tlsConfig,
	}
	var dispatcher eventing.Dispatcher = eventing.Noop{}
	var worker *eventing.Worker
	if publisher != nil {
		var startErr error
		worker, startErr = eventing.Start(publisher, settings.Eventing.QueueCapacity, settings.Eventing.PublishTimeout, serverLogger)
		if startErr != nil {
			_ = listener.Close()
			return startErr
		}
		dispatcher = worker
	}
	app := service.New(db, reports, dispatcher, settings.MaxReportBytes, serverLogger)
	server.Handler = httpapi.New(app, settings.ReadTimeout, settings.PollMaxAge, settings.MaxRequestBytes, settings.MaxReportBytes, serverLogger)
	if settings.TLS == nil {
		serverLogger.Warn("HTTP transport is unencrypted; API keys and registration tokens may be exposed in transit")
	}
	serverLogger.Info("server listening", "address", listener.Addr().String())
	if ready != nil {
		ready()
	}
	err = serve(ctx, server, listener, settings, serverLogger)
	if worker != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), settings.Eventing.ShutdownTimeout)
		defer cancel()
		if shutdownErr := worker.Shutdown(shutdown); shutdownErr != nil {
			serverLogger.Warn("event worker shutdown incomplete", "reason", "best_effort_event_loss")
		}
	}
	return err
}

func bootstrap(ctx context.Context, args []string, output io.Writer) error {
	settings, name, err := config.ParseBootstrap(args, output)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.StartupTimeout)
	defer cancel()
	db, err := database.Connect(ctx, settings.DatabaseEngine, settings.DatabaseDSN, settings.SQLiteBusyTimeout)
	if err != nil {
		return errors.New("bootstrap database initialization failed")
	}
	defer db.Close()
	result, err := db.Bootstrap(ctx, name)
	if errors.Is(err, database.ErrAlreadyBootstrapped) {
		return err
	}
	if err != nil {
		return errors.New("bootstrap transaction failed")
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		recovery, cancel := context.WithTimeout(context.Background(), settings.StartupTimeout)
		defer cancel()
		if err := db.RevokeKey(recovery, "admin", result.AdminID, result.KeyID); err != nil {
			return errors.New("bootstrap output and key revocation failed; revoke the local bootstrap key in SQL before retrying")
		}
		return errors.New("bootstrap output failed; the new key was revoked, rerun bootstrap with protected output")
	}
	return nil
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, settings config.Settings, logger *slog.Logger) (err error) {
	// ServeTLS can fail before net/http takes ownership of the listener.
	defer listener.Close()
	defer func() { err = errors.Join(err, server.Close()) }()
	done := make(chan error, 1)
	go func() {
		if settings.TLS != nil {
			done <- server.ServeTLS(listener, "", "")
		} else {
			done <- server.Serve(listener)
		}
	}()
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
