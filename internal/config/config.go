package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Settings struct {
	ListenAddress     string
	ExternalHTTPS     bool
	DevelopmentHTTP   bool
	DatabaseEngine    string
	DatabaseDSN       string
	ReportRoot        string
	PollMaxAge        time.Duration
	MaxRequestBytes   int64
	MaxReportBytes    int64
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	StartupTimeout    time.Duration
	ShutdownTimeout   time.Duration
	ProbeTimeout      time.Duration
	SQLiteBusyTimeout time.Duration
}

func Defaults() Settings {
	return Settings{
		ListenAddress: "127.0.0.1:8080", DatabaseEngine: "sqlite",
		DatabaseDSN: "registry.db", ReportRoot: "reports",
		PollMaxAge: 5 * time.Minute, MaxRequestBytes: 1 << 20, MaxReportBytes: 16 << 20,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute,
		StartupTimeout: 10 * time.Second, ShutdownTimeout: 15 * time.Second,
		ProbeTimeout: 2 * time.Second, SQLiteBusyTimeout: 5 * time.Second,
	}
}

func Parse(args []string, help io.Writer) (Settings, error) {
	s := Defaults()
	f := flag.NewFlagSet("dscregd", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	configPath := f.String("config", "", "JSON configuration file (read once at startup)")
	f.StringVar(&s.ListenAddress, "listen", s.ListenAddress, "HTTP listen IP and port")
	f.BoolVar(&s.ExternalHTTPS, "external-https", false, "HTTPS is enforced by a trusted reverse proxy")
	f.BoolVar(&s.DevelopmentHTTP, "development-http", false, "allow HTTP on a literal loopback IP only")
	databaseFlags(f, &s)
	f.StringVar(&s.ReportRoot, "report-root", s.ReportRoot, "existing private report directory")
	f.DurationVar(&s.PollMaxAge, "poll-max-age", s.PollMaxAge, "agent cache max age (whole seconds)")
	f.Int64Var(&s.MaxRequestBytes, "max-request-bytes", s.MaxRequestBytes, "administration/configuration request limit")
	f.Int64Var(&s.MaxReportBytes, "max-report-bytes", s.MaxReportBytes, "complete report upload request limit")
	f.DurationVar(&s.ReadHeaderTimeout, "read-header-timeout", s.ReadHeaderTimeout, "HTTP header read timeout")
	f.DurationVar(&s.ReadTimeout, "read-timeout", s.ReadTimeout, "HTTP request read timeout")
	f.DurationVar(&s.WriteTimeout, "write-timeout", s.WriteTimeout, "HTTP response write timeout")
	f.DurationVar(&s.IdleTimeout, "idle-timeout", s.IdleTimeout, "HTTP keep-alive idle timeout")
	f.DurationVar(&s.ShutdownTimeout, "shutdown-timeout", s.ShutdownTimeout, "graceful shutdown timeout")
	f.DurationVar(&s.ProbeTimeout, "probe-timeout", s.ProbeTimeout, "readiness response timeout")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			f.SetOutput(help)
			f.PrintDefaults()
			return s, flag.ErrHelp
		}
		return s, errors.New("invalid command-line options; use -h for help")
	}
	if f.NArg() != 0 {
		return s, errors.New("unexpected positional arguments")
	}
	if err := mergeFile(f, *configPath, &s); err != nil {
		return s, err
	}
	return s, s.Validate()
}

func databaseFlags(f *flag.FlagSet, s *Settings) {
	f.StringVar(&s.DatabaseEngine, "database-engine", s.DatabaseEngine, "database engine (sqlite or postgres)")
	f.StringVar(&s.DatabaseDSN, "database-dsn", s.DatabaseDSN, "local SQLite file path or PostgreSQL connection string")
	f.DurationVar(&s.StartupTimeout, "startup-timeout", s.StartupTimeout, "database startup timeout")
	f.DurationVar(&s.SQLiteBusyTimeout, "sqlite-busy-timeout", s.SQLiteBusyTimeout, "SQLite lock wait (whole milliseconds, at most 1m)")
}

func ParseBootstrap(args []string, help io.Writer) (Settings, string, error) {
	s := Defaults()
	name := "administrator"
	f := flag.NewFlagSet("dscregd bootstrap", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	configPath := f.String("config", "", "JSON configuration file (database settings only)")
	databaseFlags(f, &s)
	f.StringVar(&name, "name", name, "initial administrator display name (recovery reuses the first identity)")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			f.SetOutput(help)
			f.PrintDefaults()
			return s, name, flag.ErrHelp
		}
		return s, name, errors.New("invalid bootstrap options; use bootstrap -h for help")
	}
	if f.NArg() != 0 || !ValidDisplayName(name, 256) {
		return s, name, errors.New("invalid bootstrap arguments or administrator name")
	}
	if err := mergeFile(f, *configPath, &s); err != nil {
		return s, name, err
	}
	return s, name, s.validateDatabase()
}

func ValidDisplayName(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s Settings) Validate() error {
	host, port, err := net.SplitHostPort(s.ListenAddress)
	if err != nil || net.ParseIP(host) == nil {
		return errors.New("listen must contain a literal IP address and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return errors.New("listen port must be valid")
	}
	if s.ExternalHTTPS == s.DevelopmentHTTP {
		return errors.New("select exactly one of external-https or development-http")
	}
	if s.DevelopmentHTTP && !net.ParseIP(host).IsLoopback() {
		return errors.New("development-http requires a loopback listen IP")
	}
	if err := s.validateDatabase(); err != nil {
		return err
	}
	if strings.TrimSpace(s.ReportRoot) == "" {
		return errors.New("report-root must not be empty")
	}
	root, err := filepath.Abs(s.ReportRoot)
	if err != nil {
		return errors.New("invalid report-root")
	}
	if s.DatabaseEngine == "sqlite" {
		dbPath, err := filepath.Abs(s.DatabaseDSN)
		if err != nil || filepath.Clean(dbPath) == filepath.Clean(root) {
			return errors.New("report-root must be a directory distinct from the database file")
		}
	}
	if s.PollMaxAge <= 0 || s.PollMaxAge%time.Second != 0 {
		return errors.New("poll-max-age must be positive whole seconds")
	}
	if s.MaxRequestBytes <= 0 || s.MaxReportBytes < s.MaxRequestBytes {
		return errors.New("body limits must be positive and max-report-bytes must be at least max-request-bytes")
	}
	for _, limit := range []struct {
		name  string
		value time.Duration
	}{
		{"read-header-timeout", s.ReadHeaderTimeout}, {"read-timeout", s.ReadTimeout},
		{"write-timeout", s.WriteTimeout}, {"idle-timeout", s.IdleTimeout},
		{"shutdown-timeout", s.ShutdownTimeout},
		{"probe-timeout", s.ProbeTimeout},
	} {
		if limit.value <= 0 {
			return fmt.Errorf("%s must be positive", limit.name)
		}
	}
	if s.ReadHeaderTimeout > s.ReadTimeout || s.ProbeTimeout >= s.WriteTimeout {
		return errors.New("header timeout must not exceed read timeout; probe timeout must be less than write timeout")
	}
	return nil
}

func (s Settings) validateDatabase() error {
	if s.DatabaseEngine != "sqlite" && s.DatabaseEngine != "postgres" {
		return errors.New("database-engine must be sqlite or postgres")
	}
	if strings.TrimSpace(s.DatabaseDSN) == "" {
		return errors.New("database-dsn must not be empty")
	}
	if s.DatabaseEngine == "sqlite" && (strings.HasPrefix(s.DatabaseDSN, "file:") ||
		s.DatabaseDSN == ":memory:" || strings.HasPrefix(s.DatabaseDSN, `\\`) ||
		strings.HasPrefix(s.DatabaseDSN, "//")) {
		return errors.New("database-dsn must be a persistent local SQLite file path, not a URI")
	}
	if s.StartupTimeout <= 0 {
		return errors.New("startup-timeout must be positive")
	}
	if s.SQLiteBusyTimeout <= 0 || s.SQLiteBusyTimeout > time.Minute ||
		s.SQLiteBusyTimeout%time.Millisecond != 0 {
		return errors.New("sqlite-busy-timeout must be positive whole milliseconds, at most 1m")
	}
	return nil
}
