// Package diagnostics extracts safe dependency error details without logging
// driver messages, broker descriptions, connection strings or filesystem paths.
package diagnostics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"

	"github.com/Azure/go-amqp"
)

type stagedError struct {
	stage string
	err   error
}

func (e *stagedError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stagedError) Unwrap() error { return e.err }

// At preserves the original error for errors.Is/As and records a fixed stage.
func At(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &stagedError{stage, err}
}

// Details walks joined errors too: a failed close must not obscure a write
// failure. Only fixed classifications and numeric driver/OS codes are emitted.
func Details(err error) []map[string]any {
	var result []map[string]any
	var visit func(error, string)
	visit = func(err error, stage string) {
		if err == nil {
			return
		}
		if e, ok := err.(*stagedError); ok {
			visit(e.err, e.stage)
			return
		}
		if e, ok := err.(interface{ Unwrap() []error }); ok {
			for _, child := range e.Unwrap() {
				visit(child, stage)
			}
			return
		}
		detail := map[string]any{"type": fmt.Sprintf("%T", err)}
		if stage != "" {
			detail["stage"] = stage
		}
		switch {
		case errors.Is(err, context.Canceled):
			detail["kind"] = "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			detail["kind"] = "timeout"
		case errors.Is(err, os.ErrNotExist):
			detail["kind"] = "not_exist"
		case errors.Is(err, os.ErrPermission):
			detail["kind"] = "permission"
		case errors.Is(err, io.ErrShortWrite):
			detail["kind"] = "short_write"
		case errors.Is(err, io.EOF):
			detail["kind"] = "eof"
		case errors.Is(err, io.ErrUnexpectedEOF):
			detail["kind"] = "unexpected_eof"
		case errors.Is(err, driver.ErrBadConn):
			detail["kind"] = "bad_connection"
		case errors.Is(err, sql.ErrTxDone):
			detail["kind"] = "transaction_done"
		case err.Error() == "sql: database is closed":
			detail["kind"] = "database_closed"
		case errors.Is(err, net.ErrClosed), errors.Is(err, os.ErrClosed), errors.Is(err, sql.ErrConnDone):
			detail["kind"] = "closed"
		default:
			detail["kind"] = "dependency_error"
		}
		var errno syscall.Errno
		if errors.As(err, &errno) {
			detail["errno"] = uint64(errno)
			detail["reason"] = errno.Error()
		}
		var pg interface{ SQLState() string }
		// SQLSTATE is a five-character protocol code, never driver free text.
		if errors.As(err, &pg) {
			code := pg.SQLState()
			if validSQLState(code) {
				detail["sqlstate"] = code
			}
		}
		var broker *amqp.Error
		if errors.As(err, &broker) {
			// Never log arbitrary broker symbols, descriptions or Info data.
			switch broker.Condition {
			case amqp.ErrCondUnauthorizedAccess, amqp.ErrCondNotFound,
				amqp.ErrCondNotAllowed, amqp.ErrCondInternalError,
				amqp.ErrCondResourceDeleted, amqp.ErrCondResourceLimitExceeded,
				amqp.ErrCondResourceLocked, amqp.ErrCondConnectionForced,
				amqp.ErrCondDetachForced, amqp.ErrCondMessageSizeExceeded:
				detail["amqp_condition"] = string(broker.Condition)
			}
		}
		var sqlite interface{ Code() int }
		if errors.As(err, &sqlite) {
			detail["sqlite_code"] = sqlite.Code()
		}
		var network net.Error
		if errors.As(err, &network) && network.Timeout() {
			detail["kind"] = "timeout"
		}
		// Unwrap path and network errors; their Error strings contain private data.
		if child := errors.Unwrap(err); child != nil {
			visit(child, stage)
			return
		}
		result = append(result, detail)
	}
	visit(err, "")
	return result
}

func validSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for _, c := range code {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}
