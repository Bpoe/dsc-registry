package cli

import (
	"errors"
	"strings"
	"testing"
)

type failingPrivateFile struct {
	failure        string
	synced, closed bool
}

func (f *failingPrivateFile) Write(data []byte) (int, error) {
	switch f.failure {
	case "write":
		return 0, errors.New("sensitive write failure")
	case "short-write":
		return len(data) - 1, nil
	default:
		return len(data), nil
	}
}

func (f *failingPrivateFile) Sync() error {
	f.synced = true
	if f.failure == "sync" {
		return errors.New("sensitive sync failure")
	}
	return nil
}

func (f *failingPrivateFile) Close() error {
	f.closed = true
	if f.failure == "close" {
		return errors.New("sensitive close failure")
	}
	return nil
}

func TestIssuedCredentialSaveFailuresIdentifyRecovery(t *testing.T) {
	for _, resource := range []string{"api-key", "registration-token"} {
		for _, failure := range []string{"write", "short-write", "sync", "close", "missing-secret"} {
			t.Run(resource+"/"+failure, func(t *testing.T) {
				file := &failingPrivateFile{failure: failure}
				field := "key"
				if resource == "registration-token" {
					field = "token"
				}
				value := map[string]any{"keyId": "k_issued", field: "private-credential", "expiresAt": "2026-10-24T00:00:00Z"}
				if failure == "missing-secret" {
					delete(value, field)
				}
				err := saveIssuedCredential(file, value, resource, field)
				if err == nil || !file.closed || failure != "missing-secret" && !file.synced {
					t.Fatalf("save did not fail and close explicitly: %v %#v", err, file)
				}
				message := err.Error()
				if strings.Contains(message, "private-credential") || strings.Contains(message, "sensitive") {
					t.Fatalf("save error exposed sensitive data: %s", message)
				}
				if resource == "api-key" {
					if !strings.Contains(message, "k_issued") || !strings.Contains(message, "revoke") {
						t.Fatalf("save error omitted issued key recovery: %s", message)
					}
				} else if !strings.Contains(message, "cannot be revoked") || !strings.Contains(message, "2026-10-24T00:00:00Z") {
					t.Fatalf("save error omitted token lifetime: %s", message)
				}
			})
		}
	}
}
