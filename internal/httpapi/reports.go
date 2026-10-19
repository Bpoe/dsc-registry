package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/report"
)

func (a *adminAPI) uploadReport(w http.ResponseWriter, r *http.Request, id, name string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "Report uploads accept no query parameters")
		return
	}
	body, ok := readJSONBody(w, r, a.maxReportBytes)
	if !ok {
		return
	}
	upload, err := report.Parse(body, name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_report", "Expected a valid wrapped dscd v1 report for this managed configuration")
		return
	}
	storageFailure := false
	previous, err := a.store.PublishReport(r.Context(), id, name, upload, func() (string, error) {
		start := time.Now()
		path, err := a.reports.Write(r.Context(), upload.Result)
		storageFailure = err != nil
		outcome := "written"
		if err != nil {
			outcome = "write_error"
		}
		a.logger.Info("report write", "outcome", outcome, "duration", time.Since(start))
		return path, err
	})
	if err != nil {
		if storageFailure {
			a.reportStorageError(w)
		} else {
			a.reportError(w, err)
		}
		return
	}
	if previous != "" {
		if err := a.store.RetireReport(r.Context(), previous, a.reports.Remove); err != nil {
			// Publication succeeded; retry must still acknowledge the persisted
			// upload ID. Cleanup failure is observable but cannot undo it.
			a.logger.Warn("report retirement failed; offline orphan cleanup required")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *adminAPI) configurationReport(w http.ResponseWriter, r *http.Request, id, name, kind string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "Report retrieval accepts no query parameters")
		return
	}
	if kind == "status" {
		status, err := a.store.GetConfigurationStatus(r.Context(), id, name)
		if err != nil {
			a.reportError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
		return
	}
	var content []byte
	storageFailure := false
	err := a.store.ReadReport(r.Context(), id, name, func(path string) error {
		var err error
		content, err = a.reports.Read(r.Context(), path, a.maxReportBytes)
		storageFailure = err != nil
		return err
	})
	if err != nil {
		if storageFailure {
			a.reportStorageError(w)
		} else {
			a.reportError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (a *adminAPI) reportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, database.ErrStaleRevision):
		writeError(w, http.StatusConflict, "stale_configuration_revision", "Report configuration revision is no longer current")
	case errors.Is(err, database.ErrReportNotFound):
		writeError(w, http.StatusNotFound, "report_not_found", "No report has been received")
	default:
		a.configurationError(w, err)
	}
}

func (a *adminAPI) reportStorageError(w http.ResponseWriter) {
	a.logger.Error("report storage operation failed")
	writeError(w, http.StatusServiceUnavailable, "report_storage_unavailable", "Report storage operation unavailable")
}
