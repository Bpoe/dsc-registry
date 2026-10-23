package client

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newClient(t *testing.T, server string) *Client {
	t.Helper()
	client, err := New(server, "sensitive-key", time.Second*3, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRequestEscapingAuthenticationAndRawContent(t *testing.T) {
	raw := []byte("{ \"future\": [9007199254740993, 1e9999], \"null\": null }\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/proxy/v1/machines/a%2Fb%3F%25" || r.URL.Query().Get("name") != "secret + & ?" {
			t.Errorf("incorrect request URL: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "ApiKey sensitive-key" || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("Content-Type") != "application/json" || r.Method != http.MethodPut {
			t.Errorf("incorrect request method/headers")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(`{"template":{"n":12345678901234567890}}`)) {
			t.Errorf("incorrect body: %s", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	client := newClient(t, server.URL+"/proxy/")
	data, status, err := client.Request(context.Background(), http.MethodPut,
		"/v1/machines/"+url.PathEscape("a/b?%"), url.Values{"name": {"secret + & ?"}},
		json.RawMessage(`{"template":{"n":12345678901234567890}}`))
	if err != nil || status != http.StatusCreated || !bytes.Equal(data, raw) {
		t.Fatalf("%s, %d, %v", data, status, err)
	}
}

func TestResolveMachine(t *testing.T) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	for _, name := range []string{id, "Case & café /"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if name == id {
					if r.URL.Path != "/v1/machines/"+id || r.URL.RawQuery != "" {
						t.Errorf("bad UUID lookup: %s", r.URL)
					}
				} else if r.URL.Path != "/v1/machines" || r.URL.Query().Get("name") != name || len(r.URL.Query()) != 1 {
					t.Errorf("bad exact lookup: %s", r.URL)
				}
				_, _ = io.WriteString(w, `{"id":"`+id+`","configurationRevision":9007199254740993,"unknown":null}`)
			}))
			defer server.Close()
			machine, err := newClient(t, server.URL).ResolveMachine(context.Background(), name)
			if err != nil || machine["id"] != id || machine["configurationRevision"] != json.Number("9007199254740993") {
				t.Fatalf("%#v, %v", machine, err)
			}
			if _, exists := machine["unknown"]; !exists {
				t.Fatal("lost additive field")
			}
		})
	}
}

func TestPagination(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("limit") != "7" {
			t.Error("lost limit")
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{"machines":[{"id":"a"}],"nextCursor":"cursor+&","additive":12345678901234567890}`)
		case "cursor+&":
			_, _ = io.WriteString(w, `{"machines":[{"id":"b"}],"future":{"preserved":true}}`)
		default:
			t.Error("unexpected cursor")
		}
	}))
	defer server.Close()
	client := newClient(t, server.URL)
	first, err := client.List(context.Background(), "/v1/machines", "machines", 7, "", false)
	if err != nil || calls != 1 || first["nextCursor"] != "cursor+&" {
		t.Fatalf("%#v, %v, calls %d", first, err, calls)
	}
	calls = 0
	all, err := client.List(context.Background(), "/v1/machines", "machines", 7, "", true)
	if err != nil || calls != 2 || len(all["machines"].([]any)) != 2 || all["additive"] != json.Number("12345678901234567890") || all["future"] == nil {
		t.Fatalf("%#v, %v, calls %d", all, err, calls)
	}
	if _, exists := all["nextCursor"]; exists {
		t.Fatal("retained completed pagination cursor")
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, err := client.List(context.Background(), "/v1/machines", "machines", limit, "", false); err == nil {
			t.Fatal("accepted bad limit")
		}
	}
}

func TestPaginationRejectsCyclesAndInvalidShapes(t *testing.T) {
	for _, payload := range []string{
		`{"machines":[],"nextCursor":"repeat"}`, `{"machines":[],"nextCursor":42}`,
		`{"machines":null}`, `{"machines":{}}`, `{"other":[]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			_, err := newClient(t, server.URL).List(context.Background(), "/v1/machines", "machines", 10, "repeat", true)
			if err == nil {
				t.Fatal("accepted malformed pagination")
			}
		})
	}
}

func TestErrorsNeverExposeSensitiveContent(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"code":"machine_not_found","message":"sensitive-key secret-body"}}`,
		`{"error":{"code":"sensitive-key secret-body","message":"secret-body"}}`,
		"secret-body", `{"error":{"code":"first","code":"secret-body"}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, payload)
		}))
		_, status, err := newClient(t, server.URL).Request(context.Background(), http.MethodGet, "/v1/machines", nil, nil)
		server.Close()
		var failure *Error
		if !errors.As(err, &failure) || status != 404 || failure.Status != 404 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("unsafe error: %v", err)
		}
		if strings.Contains(payload, `"machine_not_found"`) && failure.Code != "machine_not_found" {
			t.Fatal("lost stable error code")
		}
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	address := closed.URL
	closed.Close()
	_, _, err := newClient(t, address).Request(context.Background(), http.MethodGet, "/v1/machines", url.Values{"name": {"secret-query"}}, nil)
	if err == nil || strings.Contains(err.Error(), "secret-query") || strings.Contains(err.Error(), address) {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		t.Error("followed cross-origin redirect")
	}))
	defer destination.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		for _, external := range []bool{false, true} {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				target := "/destination"
				if external {
					target = destination.URL
				}
				http.Redirect(w, r, target, code)
			}))
			_, status, err := newClient(t, server.URL).Request(context.Background(), http.MethodPost, "/v1/machines", nil, map[string]any{"name": "host"})
			server.Close()
			if err == nil || status != code || calls != 1 || destinationCalls.Load() != 0 {
				t.Fatalf("redirect %d: status %d, calls %d, %v", code, status, calls, err)
			}
		}
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := New(server.URL, "", 20*time.Millisecond, "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Request(context.Background(), http.MethodGet, "/v1/machines", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = client.Request(ctx, http.MethodGet, "/v1/machines", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
}

func TestBodyReadTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := New(server.URL, "", 20*time.Millisecond, "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, status, err := client.Request(context.Background(), http.MethodGet, "/v1/machines", nil, nil)
	if status != 200 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected body timeout: status %d, %v", status, err)
	}
}

func TestResponseValidationAndHealth(t *testing.T) {
	for _, payload := range []string{`null`, `[]`, `{"n":1,"n":2}`, `{"n":1} {}`, `{"s":"\ud800"}`, "{\"s\":\"\xff\"}"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, payload)
		}))
		_, _, err := newClient(t, server.URL).Request(context.Background(), http.MethodGet, "/v1/machines", nil, nil)
		server.Close()
		if err == nil {
			t.Fatalf("accepted %q", payload)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/ready":
			_, _ = io.WriteString(w, "ready\n")
		case "/large":
			_, _ = io.WriteString(w, `{"text":"`+strings.Repeat("x", maxResponseBytes)+`"}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	client := newClient(t, server.URL)
	client.http.Timeout = 30 * time.Second
	data, _, err := client.Request(context.Background(), http.MethodGet, "/health/ready", nil, nil)
	if err != nil || string(data) != "ready\n" {
		t.Fatalf("%s, %v", data, err)
	}
	if data, status, err := client.Request(context.Background(), http.MethodDelete, "/delete", nil, nil); err != nil || status != 204 || len(data) != 0 {
		t.Fatalf("%s, %d, %v", data, status, err)
	}
	if _, _, err := client.Request(context.Background(), http.MethodGet, "/large", nil, nil); err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatalf("missing size error: %v", err)
	}
}

func TestTLSAndURLValidation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	if _, _, err := newClient(t, server.URL).Request(context.Background(), http.MethodGet, "/v1/machines", nil, nil); err == nil {
		t.Fatal("trusted unconfigured certificate")
	}
	file, err := os.CreateTemp(".", "client-ca-test-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	if err := pem.Encode(file, &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct {
		ca       string
		insecure bool
	}{{file.Name(), false}, {"", true}} {
		client, err := New(server.URL, "", time.Second*3, config.ca, config.insecure)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.Request(context.Background(), http.MethodGet, "/v1/machines", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"", "/relative", "ftp://host", "https://user:secret@host", "https://host/?secret=1", "https://host/#fragment", "http://"} {
		if _, err := New(address, "", time.Second, "", false); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
	if _, err := New(server.URL, "", 0, "", false); err == nil {
		t.Fatal("accepted zero timeout")
	}
	if _, err := New(server.URL, "key\r\nbad", time.Second, "", false); err == nil {
		t.Fatal("accepted invalid header")
	}
	if _, err := New(server.URL, "", time.Second, "missing-ca-file", false); err == nil {
		t.Fatal("accepted missing CA")
	}
	for _, path := range []string{"https://other/v1", "//other/v1", "relative", "/v1?secret=1", "/v1#fragment"} {
		if _, _, err := newClient(t, server.URL).Request(context.Background(), http.MethodGet, path, nil, nil); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
