package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func testTLSCertificate(t *testing.T) (*config.TLSSettings, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	settings := &config.TLSSettings{CertFile: filepath.Join(root, "server.crt"), KeyFile: filepath.Join(root, "server.key")}
	for path, block := range map[string]*pem.Block{
		settings.CertFile: {Type: "CERTIFICATE", Bytes: der},
		settings.KeyFile:  {Type: "PRIVATE KEY", Bytes: privateKey},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return settings, roots
}

func testServerTransport(t *testing.T, server *http.Server, nativeTLS bool) (config.Settings, *http.Client, string) {
	t.Helper()
	settings := config.Defaults()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	if !nativeTLS {
		return settings, client, "http"
	}
	paths, roots := testTLSCertificate(t)
	certificate, err := tls.LoadX509KeyPair(paths.CertFile, paths.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	settings.TLS = paths
	server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	return settings, client, "https"
}

func TestRunNativeHTTPS(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		paths, roots := testTLSCertificate(t)
		root := t.TempDir()
		settings := config.Defaults()
		settings.TLS = paths
		settings.ListenAddress = "127.0.0.1:0"
		settings.DatabaseEngine, settings.DatabaseDSN = engine, dsn
		settings.ReportRoot = root
		// Exercise the actual startup-only JSON configuration path.
		data, err := json.Marshal(map[string]any{
			"listenAddress": settings.ListenAddress,
			"tls":           map[string]string{"certFile": filepath.Base(paths.CertFile), "keyFile": filepath.Base(paths.KeyFile)},
			"database":      map[string]string{"engine": engine, "dsn": dsn},
			"reportRoot":    root,
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(filepath.Dir(paths.CertFile), "config.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		settings, err = config.Parse([]string{"-config", path}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		db, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		admin, err := db.Bootstrap(context.Background(), "operator")
		if err != nil {
			t.Fatal(err)
		}
		machine, err := db.CreateMachine(context.Background(), "tls-machine")
		if err != nil {
			t.Fatal(err)
		}
		machineKey, err := db.IssueKey(context.Background(), "machine", machine.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.DB.Close(); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			address := make(chan string, 1)
			var output bytes.Buffer
			logger := slog.New(listeningLog{Handler: slog.NewJSONHandler(&output, nil), address: address})
			done := make(chan error, 1)
			go func() { done <- run(ctx, settings, logger) }()
			host := receive(t, address)
			for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
				transport := &http.Transport{
					TLSClientConfig: &tls.Config{
						RootCAs: roots, ServerName: "localhost", MinVersion: version, MaxVersion: version,
					},
					ForceAttemptHTTP2: true,
				}
				t.Cleanup(transport.CloseIdleConnections)
				client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
				for _, test := range []struct {
					path, key string
					status    int
				}{
					{"/health/ready", "", http.StatusOK},
					{"/v1/machines", "", http.StatusUnauthorized},
					{"/v1/machines", admin.Key, http.StatusOK},
					{"/v1/agent/configurations", machineKey.Key, http.StatusOK},
					{"/v1/machines", machineKey.Key, http.StatusForbidden},
				} {
					request, err := http.NewRequest(http.MethodGet, "https://"+host+test.path, nil)
					if err != nil {
						t.Fatal(err)
					}
					if test.key != "" {
						request.Header.Set("Authorization", "ApiKey "+test.key)
					}
					response, err := client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					_, readErr := io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if readErr != nil || response.StatusCode != test.status {
						t.Fatalf("%s: status=%d, error=%v", test.path, response.StatusCode, readErr)
					}
					if response.TLS == nil || response.TLS.Version != version || len(response.TLS.VerifiedChains) == 0 || response.ProtoMajor != 2 {
						t.Fatal("expected verified HTTPS with selected TLS version and default HTTP/2")
					}
				}
				transport.CloseIdleConnections()
			}
			oldTLS := &tls.Dialer{
				NetDialer: &net.Dialer{Timeout: time.Second},
				Config:    &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11},
			}
			if connection, err := oldTLS.DialContext(ctx, "tcp", host); err == nil {
				connection.Close()
				t.Fatal("TLS below 1.2 accepted")
			}
			untrusted := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "localhost"}}
			t.Cleanup(untrusted.CloseIdleConnections)
			if response, err := (&http.Client{Transport: untrusted, Timeout: 3 * time.Second}).Get("https://" + host + "/health/ready"); err == nil {
				response.Body.Close()
				t.Fatal("client accepted an untrusted certificate")
			}
			plain := &http.Transport{}
			t.Cleanup(plain.CloseIdleConnections)
			response, err := (&http.Client{Transport: plain, Timeout: 3 * time.Second}).Get("http://" + host + "/health/ready")
			if err == nil {
				response.Body.Close()
				if response.StatusCode != http.StatusBadRequest {
					t.Fatalf("plaintext request was not rejected: %d", response.StatusCode)
				}
			}
			cancel()
			if err := receive(t, done); err != nil {
				t.Fatal(err)
			}
			assertHTTPTransportWarnings(t, output.Bytes(), 0)
			assertListenerClosed(t, host)
		}
	})
}

func assertListenerClosed(t *testing.T, address string) {
	t.Helper()
	if connection, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		connection.Close()
		t.Fatal("listener remains open after shutdown or startup failure")
	}
}

func TestRunTLSInitializationFailure(t *testing.T) {
	for _, fault := range []string{"missing certificate", "missing key", "certificate directory", "key directory", "malformed certificate", "malformed key", "mismatched key", "unreadable certificate", "unreadable key"} {
		t.Run(fault, func(t *testing.T) {
			paths, _ := testTLSCertificate(t)
			switch fault {
			case "missing certificate":
				paths.CertFile = filepath.Join(t.TempDir(), "missing-certificate")
			case "missing key":
				paths.KeyFile = filepath.Join(t.TempDir(), "missing-key")
			case "certificate directory":
				paths.CertFile = t.TempDir()
			case "key directory":
				paths.KeyFile = t.TempDir()
			case "malformed certificate", "malformed key":
				path := paths.CertFile
				if fault == "malformed key" {
					path = paths.KeyFile
				}
				if err := os.WriteFile(path, []byte("sensitive-invalid-PEM"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "mismatched key":
				other, _ := testTLSCertificate(t)
				paths.KeyFile = other.KeyFile
			case "unreadable certificate", "unreadable key":
				path := paths.CertFile
				if fault == "unreadable key" {
					path = paths.KeyFile
				}
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
				if _, err := os.ReadFile(path); err == nil {
					t.Skip("platform or current account permits reading mode-000 files")
				}
			}
			settings := config.Defaults()
			settings.TLS = paths
			settings.ReportRoot = t.TempDir()
			settings.DatabaseDSN = filepath.Join(t.TempDir(), "registry.db")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			settings.ListenAddress = listener.Addr().String()
			listener.Close()
			var output bytes.Buffer
			err = run(context.Background(), settings, slog.New(slog.NewJSONHandler(&output, nil)))
			if err == nil || !strings.Contains(err.Error(), "TLS") || !strings.Contains(output.String(), "TLS initialization failed") {
				t.Fatal("TLS initialization did not fail clearly")
			}
			diagnostics := output.String() + err.Error()
			for _, secret := range []string{paths.CertFile, paths.KeyFile, "sensitive-invalid-PEM", "PRIVATE KEY", "CERTIFICATE"} {
				if strings.Contains(diagnostics, secret) {
					t.Fatal("TLS failure disclosed sensitive paths or contents")
				}
			}
			if strings.Contains(output.String(), "server listening") {
				t.Fatal("failed TLS startup announced a listener")
			}
			assertHTTPTransportWarnings(t, output.Bytes(), 0)
			if _, err := os.Stat(settings.DatabaseDSN); !os.IsNotExist(err) {
				t.Fatal("TLS initialization failure opened the database")
			}
			assertListenerClosed(t, settings.ListenAddress)
		})
	}
}

func TestServeTLSStartupFailureClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	settings := config.Defaults()
	settings.TLS = &config.TLSSettings{}
	server := &http.Server{ErrorLog: log.New(io.Discard, "", 0)}
	if err := serve(context.Background(), server, listener, settings, testLogger()); err == nil {
		t.Fatal("ServeTLS accepted missing certificate")
	}
	assertListenerClosed(t, listener.Addr().String())
}
