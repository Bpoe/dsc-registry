package telemetry

import "go.opentelemetry.io/otel/attribute"

// Resource identifiers are span attributes only.
const (
	MachineID         attribute.Key = "dscreg.machine.id"
	MachineName       attribute.Key = "dscreg.machine.name"
	ConfigurationName attribute.Key = "dscreg.configuration.name"
	ReportID          attribute.Key = "dscreg.report.id"
	ReportPath        attribute.Key = "dscreg.report.path"
)
