package diagnostics

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/Azure/go-amqp"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSafeDependencyClassifications(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		field string
		want  any
	}{
		{"permission", &os.PathError{Op: "open", Path: "private-path", Err: syscall.EACCES}, "kind", "permission"},
		{"stale_handle", syscall.ESTALE, "errno", uint64(syscall.ESTALE)},
		{"io", syscall.EIO, "errno", uint64(syscall.EIO)},
		{"missing", os.ErrNotExist, "kind", "not_exist"},
		{"deadline", context.DeadlineExceeded, "kind", "timeout"},
		{"postgres", &pgconn.PgError{Code: "53300", Message: "private-message", Detail: "private-data"}, "sqlstate", "53300"},
		{"amqp", &amqp.Error{Condition: amqp.ErrCondResourceLimitExceeded, Description: "private-description"}, "amqp_condition", "amqp:resource-limit-exceeded"},
		{"unknown", errors.New("private-message"), "kind", "dependency_error"},
		{"bad_connection", driver.ErrBadConn, "kind", "bad_connection"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := test.err
			err := At("test_stage", original)
			if !errors.Is(err, original) {
				t.Fatal("underlying error lost")
			}
			details := Details(err)
			if len(details) != 1 || details[0][test.field] != test.want || details[0]["stage"] != "test_stage" {
				t.Fatal(details)
			}
			encoded, err := json.Marshal(details)
			if err != nil || strings.Contains(string(encoded), "private") {
				t.Fatal("sensitive diagnostic", string(encoded))
			}
		})
	}
}

func TestUntrustedProtocolCodesAreNotLogged(t *testing.T) {
	for _, err := range []error{&pgconn.PgError{Code: "private-code"}, &amqp.Error{Condition: "private-condition"}} {
		encoded, _ := json.Marshal(Details(err))
		if strings.Contains(string(encoded), "private") {
			t.Fatal("untrusted protocol code logged")
		}
	}
}
