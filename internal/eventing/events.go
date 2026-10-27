package eventing

import (
	"time"

	"github.com/google/uuid"
)

const (
	MachineCreated       = "machine.created"
	MachineUpdated       = "machine.updated"
	MachineDeleted       = "machine.deleted"
	ConfigurationCreated = "configuration.created"
	ConfigurationUpdated = "configuration.updated"
	ConfigurationDeleted = "configuration.deleted"
	ReportCreated        = "report.created"
	ReportUpdated        = "report.updated"
	ReportDeleted        = "report.deleted"
)

type Identifiers struct {
	MachineID         string `json:"machineId"`
	ConfigurationName string `json:"configurationName,omitempty"`
	ReportID          string `json:"reportId,omitempty"`
}

// Event is a CloudEvents 1.0 structured JSON notification, never a document.
type Event struct {
	SpecVersion     string      `json:"specversion"`
	ID              string      `json:"id"`
	Source          string      `json:"source"`
	Type            string      `json:"type"`
	Subject         string      `json:"subject"`
	Time            time.Time   `json:"time"`
	DataContentType string      `json:"datacontenttype"`
	Data            Identifiers `json:"data"`
}

func newEvent(kind string, data Identifiers) Event {
	subject := "machines/" + data.MachineID
	if data.ConfigurationName != "" {
		subject += "/configurations/" + data.ConfigurationName
	}
	if data.ReportID != "" {
		subject = "machines/" + data.MachineID + "/reports/" + data.ReportID
	}
	return Event{
		SpecVersion: "1.0", ID: uuid.NewString(), Source: "dsc-registry",
		Type: kind, Subject: subject, Time: time.Now().UTC(),
		DataContentType: "application/json", Data: data,
	}
}

type Dispatcher interface {
	Notify(kind string, data Identifiers)
}

type Noop struct{}

func (Noop) Notify(string, Identifiers) {}
