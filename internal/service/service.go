// Package service coordinates Registry operations across SQL, report files and
// best-effort notifications. Infrastructure packages remain independent of it.
package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

var (
	ErrInvalidMachineName = errors.New("invalid machine name")
	ErrInvalidDescription = errors.New("invalid credential description")
)

// reportFiles is the focused I/O seam used to test publication failures and
// concurrent retirement. Production uses the confined filesystem store.
type reportFiles interface {
	Write(context.Context, []byte) (string, error)
	Read(context.Context, string, int64) ([]byte, error)
	Remove(string) error
}

type Service struct {
	db             *database.Store
	files          reportFiles
	events         eventing.Dispatcher
	maxReportBytes int64
	logger         *slog.Logger
	metrics        reportMetrics
	tracer         trace.Tracer
}

func New(db *database.Store, files *reportstore.Store, events eventing.Dispatcher, maxReportBytes int64, logger *slog.Logger) *Service {
	if events == nil {
		events = eventing.Noop{}
	}
	return &Service{db: db, files: files, events: events, maxReportBytes: maxReportBytes, logger: logger, metrics: newReportMetrics(), tracer: otel.Tracer("github.com/Bpoe/dsc-registry/internal/service")}
}

func (s *Service) CreateMachine(ctx context.Context, name string) (database.Machine, error) {
	if !config.ValidDisplayName(name, 256) {
		return database.Machine{}, ErrInvalidMachineName
	}
	machine, err := s.db.CreateMachine(ctx, name)
	if err == nil {
		s.events.Notify(eventing.MachineCreated, eventing.Identifiers{MachineID: machine.ID})
	}
	return machine, err
}

func (s *Service) CreateMachineWithKey(ctx context.Context, name string) (database.RegisteredMachine, error) {
	if !config.ValidDisplayName(name, 256) {
		return database.RegisteredMachine{}, ErrInvalidMachineName
	}
	machine, err := s.db.CreateMachineWithKey(ctx, name)
	if err == nil {
		s.events.Notify(eventing.MachineCreated, eventing.Identifiers{MachineID: machine.ID})
	}
	return machine, err
}

func (s *Service) DeleteMachine(ctx context.Context, id string) error {
	paths, err := s.db.DeleteMachine(ctx, id)
	if err == nil {
		s.events.Notify(eventing.MachineDeleted, eventing.Identifiers{MachineID: id})
		s.removeReportFiles(paths)
	}
	return err
}

func (s *Service) PutConfiguration(ctx context.Context, id, name string, body []byte) (database.ConfigurationResult, database.MutationOutcome, error) {
	if !desired.ValidName(name) {
		return database.ConfigurationResult{}, database.MutationUnchanged, desired.ErrInvalid
	}
	content, err := desired.Parse(body)
	if err != nil {
		return database.ConfigurationResult{}, database.MutationUnchanged, err
	}
	result, outcome, err := s.db.PutConfiguration(ctx, id, name, content)
	if err == nil {
		switch outcome {
		case database.MutationCreated:
			s.events.Notify(eventing.ConfigurationCreated, eventing.Identifiers{MachineID: id, ConfigurationName: name})
		case database.MutationUpdated:
			s.events.Notify(eventing.ConfigurationUpdated, eventing.Identifiers{MachineID: id, ConfigurationName: name})
		}
		if outcome != database.MutationUnchanged {
			s.events.Notify(eventing.MachineUpdated, eventing.Identifiers{MachineID: id})
		}
	}
	return result, outcome, err
}

func (s *Service) DeleteConfiguration(ctx context.Context, id, name string) error {
	paths, err := s.db.DeleteConfiguration(ctx, id, name)
	if err == nil {
		s.events.Notify(eventing.ConfigurationDeleted, eventing.Identifiers{MachineID: id, ConfigurationName: name})
		s.events.Notify(eventing.MachineUpdated, eventing.Identifiers{MachineID: id})
		s.removeReportFiles(paths)
	}
	return err
}

func (s *Service) Authenticate(ctx context.Context, value string) (database.Principal, error) {
	return s.db.Authenticate(ctx, value)
}

func (s *Service) GetMachine(ctx context.Context, id string) (database.Machine, error) {
	return s.db.GetMachine(ctx, id)
}

func (s *Service) GetMachineByName(ctx context.Context, name string) (database.Machine, error) {
	return s.db.GetMachineByName(ctx, name)
}

func (s *Service) ListMachines(ctx context.Context, after string, limit int) ([]database.Machine, error) {
	return s.db.ListMachines(ctx, after, limit)
}

func (s *Service) IssueKey(ctx context.Context, role, id, description string) (database.IssuedKey, error) {
	if description != "" && !config.ValidDisplayName(description, 1024) {
		return database.IssuedKey{}, ErrInvalidDescription
	}
	return s.db.IssueKey(ctx, role, id, description)
}

func (s *Service) ListKeys(ctx context.Context, role, id, after string, limit int) ([]database.Key, error) {
	return s.db.ListKeys(ctx, role, id, after, limit)
}

func (s *Service) CreateRegistrationToken(ctx context.Context, ttl int64) (database.RegistrationToken, error) {
	return s.db.CreateRegistrationToken(ctx, ttl)
}

func (s *Service) ListConfigurations(ctx context.Context, id, after string, limit int) ([]database.ConfigurationSummary, error) {
	return s.db.ListConfigurations(ctx, id, after, limit)
}

func (s *Service) GetConfiguration(ctx context.Context, id, name string) (database.ConfigurationResult, error) {
	return s.db.GetConfiguration(ctx, id, name)
}

func (s *Service) ConfigurationRevision(ctx context.Context, id string) (int64, error) {
	return s.db.ConfigurationRevision(ctx, id)
}

func (s *Service) ConfigurationSnapshot(ctx context.Context, id string) (database.ConfigurationSnapshot, error) {
	return s.db.ConfigurationSnapshot(ctx, id)
}

func (s *Service) ListReports(ctx context.Context, id, cursor string, limit int) ([]database.ReportMetadata, error) {
	return s.db.ListReports(ctx, id, cursor, limit)
}

func (s *Service) RevokeKey(ctx context.Context, role, id, keyID string) error {
	return s.db.RevokeKey(ctx, role, id, keyID)
}

func (s *Service) AuthenticateRegistrationToken(ctx context.Context, value string) error {
	return s.db.AuthenticateRegistrationToken(ctx, value)
}
