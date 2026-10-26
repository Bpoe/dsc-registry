package eventing

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/Azure/go-amqp"
	"github.com/Bpoe/dsc-registry/internal/config"
)

const contentType = "application/cloudevents+json"

// AMQP is owned exclusively by the publisher worker. Construction validates
// local credentials but does not contact the broker.
type AMQP struct {
	settings    config.EventingSettings
	options     amqp.ConnOptions
	logger      *slog.Logger
	network     net.Conn
	conn        *amqp.Conn
	sender      *amqp.Sender
	backoff     time.Duration
	nextAttempt time.Time
	connected   bool
}

func NewAMQP(settings config.EventingSettings, logger *slog.Logger) (*AMQP, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	options, err := connectionOptions(settings)
	if err != nil {
		return nil, err
	}
	return &AMQP{settings: settings, options: options, logger: logger}, nil
}

func connectionOptions(settings config.EventingSettings) (amqp.ConnOptions, error) {
	options := amqp.ConnOptions{WriteTimeout: settings.PublishTimeout}
	switch settings.Auth.Type {
	case "plain":
		password, ok := os.LookupEnv(settings.Auth.PasswordEnv)
		if !ok || password == "" {
			return options, errors.New("eventing password environment variable is unset or empty")
		}
		options.SASLType = amqp.SASLTypePlain(settings.Auth.Username, password)
	case "anonymous":
		options.SASLType = amqp.SASLTypeAnonymous()
	case "external":
		options.SASLType = amqp.SASLTypeExternal("")
	}
	endpoint, _ := url.Parse(settings.Endpoint) // Validated before construction.
	options.HostName = endpoint.Hostname()
	if endpoint.Scheme == "amqps" {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.Hostname()}
		if settings.TLS.CAFile != "" {
			pem, err := os.ReadFile(settings.TLS.CAFile)
			if err != nil {
				return options, errors.New("cannot read eventing TLS CA file")
			}
			pool, err := x509.SystemCertPool()
			if err != nil {
				return options, errors.New("cannot load system certificate authorities")
			}
			if !pool.AppendCertsFromPEM(pem) {
				return options, errors.New("invalid eventing TLS CA file")
			}
			options.TLSConfig.RootCAs = pool
		}
		if settings.TLS.CertFile != "" {
			certificate, err := tls.LoadX509KeyPair(settings.TLS.CertFile, settings.TLS.KeyFile)
			if err != nil {
				return options, errors.New("cannot load eventing TLS client certificate and key")
			}
			options.TLSConfig.Certificates = []tls.Certificate{certificate}
		}
	}
	return options, nil
}

func (p *AMQP) Publish(ctx context.Context, event Event) (err error) {
	if delay := time.Until(p.nextAttempt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer func() {
		if err == nil {
			p.backoff = 0
			p.nextAttempt = time.Time{}
			return
		}
		var connectionError *amqp.ConnError
		if errors.As(err, &connectionError) {
			p.logger.Warn("AMQP connection", "outcome", "connection_failed")
		}
		if closeErr := p.Close(); closeErr != nil {
			p.logger.Warn("AMQP connection cleanup failed")
		}
		p.backoff = nextBackoff(p.backoff)
		p.nextAttempt = time.Now().Add(p.backoff)
	}()
	if p.sender == nil {
		connect, cancel := context.WithTimeout(ctx, p.settings.ConnectTimeout)
		err = p.connect(connect)
		cancel()
		if err != nil {
			p.logger.Warn("AMQP connection", "outcome", "connect_failed")
			return err
		}
		outcome := "connected"
		if p.connected {
			outcome = "reconnected"
		}
		p.connected = true
		p.logger.Info("AMQP connection", "outcome", outcome)
	}
	message, err := eventMessage(event)
	if err != nil {
		return err
	}
	receipt, err := p.sender.SendWithReceipt(ctx, message, nil)
	if err != nil {
		return err
	}
	state, err := receipt.Wait(ctx)
	if err != nil {
		return err
	}
	return accepted(state)
}

func nextBackoff(previous time.Duration) time.Duration {
	if previous == 0 {
		return 500 * time.Millisecond
	}
	return min(previous*2, 30*time.Second)
}

func eventMessage(event Event) (*amqp.Message, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	message := amqp.NewMessage(data)
	message.Properties = &amqp.MessageProperties{MessageID: event.ID, ContentType: ptr(contentType)}
	return message, nil
}

func ptr[T any](value T) *T { return &value }

func accepted(state amqp.DeliveryState) error {
	if _, ok := state.(*amqp.StateAccepted); !ok {
		// Broker descriptions are untrusted and may contain credentials/data.
		return errors.New("AMQP delivery was not accepted")
	}
	return nil
}

func (p *AMQP) connect(ctx context.Context) error {
	endpoint, _ := url.Parse(p.settings.Endpoint)
	port := endpoint.Port()
	if port == "" {
		port = "5672"
		if endpoint.Scheme == "amqps" {
			port = "5671"
		}
	}
	address := net.JoinHostPort(endpoint.Hostname(), port)
	var err error
	if p.options.TLSConfig != nil {
		p.network, err = (&tls.Dialer{Config: p.options.TLSConfig}).DialContext(ctx, "tcp", address)
	} else {
		p.network, err = (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return errors.New("AMQP network connection failed")
	}
	// Interrupt handshake I/O even when cancellation precedes its deadline.
	network := p.network
	stop := context.AfterFunc(ctx, func() { _ = network.Close() })
	defer stop()
	p.conn, err = amqp.NewConn(ctx, p.network, &p.options)
	if err != nil {
		return errors.New("AMQP negotiation failed")
	}
	session, err := p.conn.NewSession(ctx, nil)
	if err != nil {
		return errors.New("AMQP session failed")
	}
	p.sender, err = session.NewSender(ctx, p.settings.Address, &amqp.SenderOptions{
		SettlementMode: ptr(amqp.SenderSettleModeUnsettled),
	})
	if err != nil {
		return errors.New("AMQP destination link failed")
	}
	return nil
}

func (p *AMQP) Close() error {
	var err error
	if p.network != nil {
		// Close the socket first: protocol shutdown must not wait for an
		// unavailable peer or outlive the application's shutdown deadline.
		err = p.network.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
	}
	if p.conn != nil {
		// The connection's diagnostic error reflects our socket closure.
		_ = p.conn.Close()
	}
	p.network, p.conn, p.sender = nil, nil, nil
	return err
}
