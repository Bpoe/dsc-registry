package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/diagnostics"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/report"
)

func (a *adminAPI) uploadReport(w http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(machineIDContextKey{}).(string)
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "Report uploads accept no query parameters")
		return
	}
	body, ok := readJSONBody(w, r, a.maxReportBytes)
	if !ok {
		return
	}
	upload, err := report.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_report", "Expected a valid wrapped dscd v1 report")
		return
	}
	var storageFailure error
	storageOperation := "report_read"
	publication, err := a.store.PublishReport(r.Context(), id, upload.Name, upload, func() (string, error) {
		start := time.Now()
		storageOperation = "report_write"
		path, err := a.reports.Write(r.Context(), upload.Result)
		storageFailure = err
		outcome := "written"
		if err != nil {
			outcome = "write_error"
		}
		a.logger.Info("report write", "outcome", outcome, "duration", time.Since(start))
		return path, err
	}, func(path string) ([]byte, error) {
		storageOperation = "report_read"
		content, err := a.reports.Read(r.Context(), path, a.maxReportBytes)
		storageFailure = err
		return content, err
	})
	if err != nil {
		a.removeReportFiles(publication.CleanupPaths)
		if storageFailure != nil && !reportResourceMissing(err) {
			a.reportStorageError(w, r, storageOperation, err, storageFailure)
		} else {
			a.reportError(w, r, err)
		}
		return
	}
	data := eventing.Identifiers{MachineID: id, ConfigurationName: upload.Name, ReportID: publication.Report.ID}
	switch publication.Outcome {
	case database.MutationCreated:
		a.events.Notify(eventing.ReportCreated, data)
	case database.MutationUpdated:
		a.events.Notify(eventing.ReportUpdated, data)
	}
	a.removeReportFiles(publication.CleanupPaths)
	w.Header().Set("Location", "/v1/machines/"+id+"/reports/"+publication.Report.ID)
	status := http.StatusOK
	if publication.Outcome != database.MutationUnchanged {
		status = http.StatusCreated
	}
	writeJSON(w, status, publication.Report)
}

func (a *adminAPI) removeReportFiles(paths []string) {
	for _, path := range paths {
		start := time.Now()
		if err := a.reports.Remove(path); err != nil {
			a.logger.Warn("report file cleanup failed", "dependency", "report_storage", "operation", "report_delete", "duration", time.Since(start), "error", diagnostics.Details(err))
		}
	}
}

func (a *adminAPI) machineReports(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pagination(w, r, validUUID)
	if !ok {
		return
	}
	items, err := a.store.ListReports(r.Context(), r.PathValue("id"), cursor, limit+1)
	if err != nil {
		a.reportError(w, r, err)
		return
	}
	next := ""
	if len(items) > limit {
		next = items[limit-1].ID
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, struct {
		Reports    []database.ReportMetadata `json:"reports"`
		NextCursor string                    `json:"nextCursor,omitempty"`
	}{items, next})
}

func (a *adminAPI) machineReport(w http.ResponseWriter, r *http.Request, full bool) {
	if !reportReadRequest(w, r) {
		return
	}
	var content []byte
	var read func(string) error
	var storageFailure error
	if full {
		read = func(path string) error {
			var err error
			content, err = a.reports.Read(r.Context(), path, a.maxReportBytes)
			storageFailure = err
			return err
		}
	}
	item, err := a.store.GetReport(r.Context(), r.PathValue("id"), r.PathValue("reportId"), read)
	if err != nil {
		if storageFailure != nil && !reportResourceMissing(err) {
			a.reportStorageError(w, r, "report_read", err, storageFailure)
		} else {
			a.reportError(w, r, err)
		}
		return
	}
	if full {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	} else {
		writeJSON(w, http.StatusOK, item)
	}
}

func (a *adminAPI) machineStatus(w http.ResponseWriter, r *http.Request) {
	if !reportReadRequest(w, r) {
		return
	}
	status, err := a.store.GetMachineStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		a.reportError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *adminAPI) configurationStatus(w http.ResponseWriter, r *http.Request) {
	if !reportReadRequest(w, r) {
		return
	}
	status, err := a.store.GetConfigurationStatus(r.Context(), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		a.reportError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *adminAPI) configurationReport(w http.ResponseWriter, r *http.Request) {
	if !reportReadRequest(w, r) {
		return
	}
	var content []byte
	var storageFailure error
	err := a.store.ReadReport(r.Context(), r.PathValue("id"), r.PathValue("name"), func(path string) error {
		var err error
		content, err = a.reports.Read(r.Context(), path, a.maxReportBytes)
		storageFailure = err
		return err
	})
	if err != nil {
		if storageFailure != nil && !reportResourceMissing(err) {
			a.reportStorageError(w, r, "report_read", err, storageFailure)
		} else {
			a.reportError(w, r, err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func reportResourceMissing(err error) bool {
	return errors.Is(err, database.ErrNotFound) || errors.Is(err, database.ErrConfigurationNotFound) ||
		errors.Is(err, database.ErrReportNotFound)
}

func reportReadRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "Report retrieval accepts no query parameters")
		return false
	}
	return true
}

func (a *adminAPI) reportError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrUploadConflict):
		writeError(w, http.StatusConflict, "upload_id_conflict", "Upload ID already identifies a different report")
	case errors.Is(err, database.ErrReportCursor):
		writeError(w, http.StatusBadRequest, "invalid_pagination", "Report cursor does not belong to this machine")
	case errors.Is(err, database.ErrReportNotFound):
		writeError(w, http.StatusNotFound, "report_not_found", "No matching report has been received")
	default:
		a.configurationError(w, r, err)
	}
}

func (a *adminAPI) reportStorageError(w http.ResponseWriter, r *http.Request, operation string, err, storageErr error) {
	// Reselecting metadata after a missing file can itself fail. Diagnose the
	// final cause while retaining the existing storage-path HTTP response.
	if errors.Is(err, storageErr) {
		a.logDependencyFailure(r, "report_storage", operation, err)
	} else {
		a.logDependencyFailure(r, "database", "query", err)
	}
	writeError(w, http.StatusServiceUnavailable, "report_storage_unavailable", "Report storage operation unavailable")
}
