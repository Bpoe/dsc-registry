package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type EventingSettings struct {
	Type            string
	Endpoint        string
	Address         string
	Auth            EventingAuth
	TLS             EventingTLS
	QueueCapacity   int
	ConnectTimeout  time.Duration
	PublishTimeout  time.Duration
	ShutdownTimeout time.Duration
}

type EventingAuth struct {
	Type        string
	Username    string
	PasswordEnv string
}

type EventingTLS struct {
	CAFile   string
	CertFile string
	KeyFile  string
}

func EventingDefaults() EventingSettings {
	return EventingSettings{
		Type: "amqp1", Auth: EventingAuth{Type: "anonymous"}, QueueCapacity: 1000,
		ConnectTimeout: 5 * time.Second, PublishTimeout: 10 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}
}

func (s EventingSettings) Validate() error {
	if s.Type != "amqp1" {
		return errors.New("eventing.type must be amqp1")
	}
	endpoint, err := url.Parse(s.Endpoint)
	if err != nil || (endpoint.Scheme != "amqp" && endpoint.Scheme != "amqps") ||
		endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.Opaque != "" {
		return errors.New("eventing.endpoint requires an amqp:// or amqps:// host without credentials, query or address path")
	}
	if strings.HasSuffix(endpoint.Host, ":") {
		return errors.New("invalid eventing.endpoint port")
	}
	if strings.Contains(endpoint.Host, ":") {
		if _, _, err := net.SplitHostPort(endpoint.Host); err != nil && !strings.HasSuffix(endpoint.Host, "]") {
			return errors.New("invalid eventing.endpoint host or port")
		}
	}
	if endpoint.Port() != "" {
		port, err := strconv.Atoi(endpoint.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("invalid eventing.endpoint port")
		}
	}
	if strings.TrimSpace(s.Address) == "" {
		return errors.New("eventing.address must not be empty")
	}
	if s.QueueCapacity <= 0 || s.QueueCapacity > 1000000 {
		return errors.New("eventing.queueCapacity must be between 1 and 1000000")
	}
	if s.ConnectTimeout <= 0 || s.PublishTimeout <= 0 || s.ShutdownTimeout <= 0 {
		return errors.New("eventing timeouts must be positive")
	}
	if (s.TLS.CertFile == "") != (s.TLS.KeyFile == "") {
		return errors.New("eventing.tls.certFile and keyFile must both be specified")
	}
	if endpoint.Scheme != "amqps" && (s.TLS.CAFile != "" || s.TLS.CertFile != "" || s.Auth.Type == "external") {
		return errors.New("eventing TLS settings and external authentication require amqps://")
	}
	switch s.Auth.Type {
	case "plain":
		if s.Auth.Username == "" || s.Auth.PasswordEnv == "" {
			return errors.New("eventing plain authentication requires username and passwordEnv")
		}
	case "anonymous", "external":
		if s.Auth.Username != "" || s.Auth.PasswordEnv != "" {
			return errors.New("eventing credentials require plain authentication")
		}
		if s.Auth.Type == "external" && s.TLS.CertFile == "" {
			return errors.New("eventing external authentication requires a client certificate and key")
		}
	default:
		return errors.New("eventing.auth.type must be plain, anonymous or external")
	}
	return nil
}
