package eventing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/go-amqp"
	"github.com/Bpoe/dsc-registry/internal/config"
)

func amqpSettings() config.EventingSettings {
	s := config.EventingDefaults()
	s.Endpoint, s.Address = "amqp://127.0.0.1:5672", "/queues/events"
	return s
}

func TestAMQPMessageAndDeliveryOutcomes(t *testing.T) {
	event := newEvent(ReportCreated, Identifiers{MachineID: "machine", ConfigurationName: "inventory", ReportID: "report"})
	message, err := eventMessage(event)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var decoded amqp.Message
	if err := decoded.UnmarshalBinary(encoded); err != nil {
		t.Fatal(err)
	}
	var result Event
	if err := json.Unmarshal(decoded.GetData(), &result); err != nil || result != event ||
		decoded.Properties.MessageID != event.ID || *decoded.Properties.ContentType != contentType {
		t.Fatal("incorrect structured CloudEvent encoding", err)
	}
	for _, state := range []amqp.DeliveryState{nil, &amqp.StateAccepted{}, &amqp.StateRejected{}, &amqp.StateReleased{}, &amqp.StateModified{}, &amqp.StateReceived{}} {
		_, success := state.(*amqp.StateAccepted)
		if err := accepted(state); (err == nil) != success {
			t.Fatalf("delivery outcome incorrectly interpreted: %T, %v", state, err)
		}
	}
}

func testCertificate(t *testing.T) (config.EventingTLS, *tls.Config) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "event-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	dir := t.TempDir()
	files := config.EventingTLS{CAFile: filepath.Join(dir, "ca.pem"), CertFile: filepath.Join(dir, "client.pem"), KeyFile: filepath.Join(dir, "client.key")}
	for path, data := range map[string][]byte{files.CAFile: certPEM, files.CertFile: certPEM, files.KeyFile: keyPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return files, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: roots}
}

// A minimal SASL peer verifies the actual negotiation bytes without requiring a
// broker. It deliberately closes after SASL-init; no AMQP session is established.
func saslPeer(t *testing.T, settings config.EventingSettings, serverTLS *tls.Config) ([]byte, error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if serverTLS != nil {
		listener = tls.NewListener(listener, serverTLS)
	}
	defer listener.Close()
	result := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- nil
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		header := make([]byte, 8)
		if _, err := io.ReadFull(conn, header); err != nil || !bytes.Equal(header, []byte{'A', 'M', 'Q', 'P', 3, 1, 0, 0}) {
			result <- nil
			return
		}
		if _, err := conn.Write(header); err != nil {
			result <- nil
			return
		}
		mechanism := strings.ToUpper(settings.Auth.Type)
		body := []byte{0, 0x53, 0x40, 0xc0, byte(6 + len(mechanism)), 1, 0xe0, byte(3 + len(mechanism)), 1, 0xa3, byte(len(mechanism))}
		body = append(body, mechanism...)
		frame := make([]byte, 8)
		binary.BigEndian.PutUint32(frame, uint32(len(body)+8))
		frame[4], frame[5] = 2, 1
		if _, err := conn.Write(append(frame, body...)); err != nil {
			result <- nil
			return
		}
		if _, err := io.ReadFull(conn, header); err != nil {
			result <- nil
			return
		}
		size := binary.BigEndian.Uint32(header)
		if size < 8 || size > 65536 {
			result <- nil
			return
		}
		body = make([]byte, size-8)
		if _, err := io.ReadFull(conn, body); err != nil {
			result <- nil
			return
		}
		result <- body
	}()
	scheme := "amqp"
	if serverTLS != nil {
		scheme = "amqps"
	}
	settings.Endpoint = scheme + "://" + listener.Addr().String()
	publisher, err := NewAMQP(settings, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = publisher.Publish(ctx, newEvent(MachineCreated, Identifiers{}))
	_ = publisher.Close()
	return waitFor(t, result), err
}

func TestAMQPSASLAuthenticationAndVerifiedClientTLS(t *testing.T) {
	for _, mechanism := range []string{"anonymous", "plain", "external"} {
		t.Run(mechanism, func(t *testing.T) {
			settings := amqpSettings()
			settings.Auth.Type = mechanism
			var serverTLS *tls.Config
			if mechanism == "plain" {
				t.Setenv("DSCREGISTRY_UNIT_AMQP_PASSWORD", "private-test-password")
				settings.Auth.Username, settings.Auth.PasswordEnv = "test-user", "DSCREGISTRY_UNIT_AMQP_PASSWORD"
			}
			if mechanism == "external" {
				settings.TLS, serverTLS = testCertificate(t)
				serverTLS.ClientAuth = tls.RequireAndVerifyClientCert
			}
			frame, err := saslPeer(t, settings, serverTLS)
			if err == nil || !bytes.Contains(frame, []byte(strings.ToUpper(mechanism))) {
				t.Fatalf("SASL mechanism not negotiated: %x, %v", frame, err)
			}
			if mechanism == "plain" && (!bytes.Contains(frame, []byte("test-user")) || !bytes.Contains(frame, []byte("private-test-password"))) {
				t.Fatal("SASL PLAIN credentials not transmitted")
			}
			if strings.Contains(err.Error(), "private-test-password") {
				t.Fatal("connection error disclosed credentials")
			}
		})
	}
}

func TestAMQPRejectsUntrustedTLSCertificate(t *testing.T) {
	settings := amqpSettings()
	_, serverTLS := testCertificate(t)
	if frame, err := saslPeer(t, settings, serverTLS); err == nil || frame != nil {
		t.Fatal("untrusted certificate was accepted")
	}
}

func TestAMQPLocalConfigurationErrorsAndNoEagerDial(t *testing.T) {
	settings := amqpSettings()
	settings.Endpoint = "amqps://127.0.0.1:1"
	publisher, err := NewAMQP(settings, nil)
	if err != nil || publisher.network != nil || publisher.conn != nil {
		t.Fatal("construction connected to broker", err)
	}
	settings.Auth = config.EventingAuth{Type: "plain", Username: "registry", PasswordEnv: "DSCREGISTRY_UNIT_AMQP_MISSING"}
	t.Setenv(settings.Auth.PasswordEnv, "")
	if _, err := NewAMQP(settings, nil); err == nil {
		t.Fatal("missing password accepted")
	}
	settings.Auth.Type, settings.Auth.Username, settings.Auth.PasswordEnv = "anonymous", "", ""
	for _, files := range []config.EventingTLS{
		{CAFile: filepath.Join(t.TempDir(), "private-secret")},
		{CertFile: "private-secret", KeyFile: "private-secret"},
	} {
		settings.TLS = files
		if _, err := NewAMQP(settings, nil); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("bad credentials accepted or leaked", err)
		}
	}
}

func TestAMQPConnectTimeoutAndBackoff(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
	}()
	settings := amqpSettings()
	settings.Endpoint, settings.ConnectTimeout = "amqp://"+listener.Addr().String(), 20*time.Millisecond
	var logs bytes.Buffer
	publisher, err := NewAMQP(settings, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := publisher.Publish(ctx, newEvent(MachineCreated, Identifiers{})); err == nil || publisher.network != nil {
		t.Fatal("connection timeout failed", err)
	}
	waitFor(t, peerDone)
	if publisher.backoff != 500*time.Millisecond || !strings.Contains(logs.String(), `"outcome":"connect_failed"`) {
		t.Fatal("missing connection failure backoff/signal")
	}
	attempt := publisher.nextAttempt
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := publisher.Publish(canceled, Event{}); !errors.Is(err, context.Canceled) || !publisher.nextAttempt.Equal(attempt) {
		t.Fatal("backoff wait ignored cancellation or retried", err)
	}
	backoff := time.Duration(0)
	for range 20 {
		backoff = nextBackoff(backoff)
		if backoff <= 0 || backoff > 30*time.Second {
			t.Fatal("backoff is unbounded", backoff)
		}
	}
	if backoff != 30*time.Second {
		t.Fatal("backoff did not reach its bound")
	}
}
