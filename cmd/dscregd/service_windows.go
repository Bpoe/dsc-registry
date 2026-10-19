package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/Bpoe/dsc-registry/internal/config"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

func platformService(args []string) (bool, error) {
	service, err := svc.IsWindowsService()
	if err != nil {
		return true, err
	}
	if !service {
		return false, nil
	}
	events, err := eventlog.Open("dscregd")
	if err != nil {
		return true, err
	}
	defer events.Close()
	logger := slog.New(slog.NewJSONHandler(serviceLog{events}, nil))
	handler := &serviceHandler{start: func(ctx context.Context, ready func()) error {
		settings, err := config.Parse(args, io.Discard)
		if err != nil {
			logger.Error("invalid service configuration", "reason", "invalid_configuration")
			return err
		}
		err = runWithReady(ctx, settings, logger, ready)
		if err != nil {
			logger.Error("service stopped", "reason", "startup_or_shutdown_failure")
		}
		return err
	}}
	return true, svc.Run("dscregd", handler)
}

type serviceLog struct{ events *eventlog.Log }

func (w serviceLog) Write(p []byte) (int, error) {
	if err := w.events.Info(1, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

type serviceHandler struct {
	start func(context.Context, func()) error
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	status := svc.Status{State: svc.StartPending, WaitHint: 30000, CheckPoint: 1}
	changes <- status
	go func() { done <- h.start(ctx, func() { close(ready) }) }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ready:
			ready = nil
			if status.State == svc.StartPending {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				changes <- status
			}
		case err := <-done:
			if err != nil && !(ctx.Err() != nil && errors.Is(err, context.Canceled)) {
				return true, 1
			}
			return false, 0
		case request, ok := <-requests:
			if !ok || request.Cmd == svc.Stop || request.Cmd == svc.Shutdown {
				requests = nil
				status = svc.Status{State: svc.StopPending, WaitHint: 30000, CheckPoint: 1}
				changes <- status
				cancel()
			} else if request.Cmd == svc.Interrogate {
				changes <- status
			}
		case <-ticker.C:
			if status.State == svc.StartPending || status.State == svc.StopPending {
				status.CheckPoint++
				changes <- status
			}
		}
	}
}
