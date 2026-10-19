package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/windows/svc"
)

func TestServiceLifecycle(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			requests := make(chan svc.ChangeRequest, 1)
			changes := make(chan svc.Status, 10)
			started, drain := make(chan struct{}), make(chan struct{})
			handler := &serviceHandler{start: func(ctx context.Context, ready func()) error {
				<-started
				ready()
				<-ctx.Done()
				<-drain
				return nil
			}}
			result := make(chan uint32, 1)
			go func() {
				_, code := handler.Execute(nil, requests, changes)
				result <- code
			}()
			if status := receive(t, changes); status.State != svc.StartPending {
				t.Fatalf("startup: %+v", status)
			}
			close(started)
			if status := receive(t, changes); status.State != svc.Running || status.Accepts != svc.AcceptStop|svc.AcceptShutdown {
				t.Fatalf("running: %+v", status)
			}
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			if status := receive(t, changes); status.State != svc.Running {
				t.Fatalf("interrogate: %+v", status)
			}
			requests <- svc.ChangeRequest{Cmd: command}
			if status := receive(t, changes); status.State != svc.StopPending {
				t.Fatalf("stopping: %+v", status)
			}
			select {
			case <-result:
				t.Fatal("returned before draining")
			default:
			}
			close(drain)
			if code := receive(t, result); code != 0 {
				t.Fatalf("exit code: %d", code)
			}
		})
	}
}

func TestServiceFailure(t *testing.T) {
	for _, afterReady := range []bool{false, true} {
		handler := &serviceHandler{start: func(_ context.Context, ready func()) error {
			if afterReady {
				ready()
			}
			return errors.New("dependency failure")
		}}
		changes := make(chan svc.Status, 10)
		specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), changes)
		if !specific || code == 0 {
			t.Fatalf("failure reported as success: %v %d", specific, code)
		}
	}
}

func TestServiceStopDuringStartup(t *testing.T) {
	requests := make(chan svc.ChangeRequest, 1)
	changes := make(chan svc.Status, 10)
	finish := make(chan struct{})
	handler := &serviceHandler{start: func(ctx context.Context, ready func()) error {
		<-ctx.Done()
		ready()
		<-finish
		return ctx.Err()
	}}
	done := make(chan uint32, 1)
	go func() {
		_, code := handler.Execute(nil, requests, changes)
		done <- code
	}()
	if status := receive(t, changes); status.State != svc.StartPending {
		t.Fatalf("startup: %+v", status)
	}
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	if status := receive(t, changes); status.State != svc.StopPending {
		t.Fatalf("stopping during startup: %+v", status)
	}
	close(finish)
	if code := receive(t, done); code != 0 {
		t.Fatalf("exit: %d", code)
	}
	close(changes)
	for status := range changes {
		if status.State == svc.Running {
			t.Fatal("reported running after stop")
		}
	}
}
