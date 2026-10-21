package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
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
		writeError(w, http.StatusBadRequest, "invalid_report", "Expected a valid wrapped dscd v1 report")
		return
	}
	storageFailure := false
	publication, err := a.store.PublishReport(r.Context(), id, upload.Name, upload, func() (string, error) {
		start := time.Now()
		path, err := a.reports.Write(r.Context(), upload.Result)
		var cleanupErr *reportstore.CleanupError
		if errors.As(err, &cleanupErr) {
			a.logger.Warn("report file cleanup failed", "path", cleanupErr.Path, "error", cleanupErr.Err)
		}
		storageFailure = err != nil
		outcome := "written"
		if err != nil {
			outcome = "write_error"
		}
		a.logger.Info("report write", "outcome", outcome, "duration", time.Since(start))
		return path, err
	}, func(path string) ([]byte, error) {
		content, err := a.reports.Read(r.Context(), path, a.maxReportBytes)
		storageFailure = err != nil
		return content, err
	})
	a.removeReportFiles(publication.CleanupPaths)
	if err != nil {
		if storageFailure && !reportResourceMissing(err) {
			a.reportStorageError(w)
		} else {
			a.reportError(w, err)
		}
		return
	}
	if name != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Location", "/v1/machines/"+id+"/reports/"+publication.Report.ID)
	status := http.StatusOK
	if publication.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, publication.Report)
}

func (a *adminAPI) removeReportFiles(paths []string) {
	for _, path := range paths {
		if err := a.reports.Remove(path); err != nil {
			a.logger.Warn("report file cleanup failed", "path", path, "error", err)
		}
	}
}

func (a *adminAPI) machineReports(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	limit, cursor, ok := pagination(w, r, validUUID)
	if !ok {
		return
	}
	items, err := a.store.ListReports(r.Context(), id, cursor, limit+1)
	if err != nil {
		a.reportError(w, err)
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

func (a *adminAPI) machineReport(w http.ResponseWriter, r *http.Request, id, reportID string, full bool) {
	if !reportReadRequest(w, r) {
		return
	}
	var content []byte
	var read func(string) error
	storageFailure := false
	if full {
		read = func(path string) error {
			var err error
			content, err = a.reports.Read(r.Context(), path, a.maxReportBytes)
			storageFailure = err != nil
			return err
		}
	}
	item, err := a.store.GetReport(r.Context(), id, reportID, read)
	if err != nil {
		if storageFailure && !reportResourceMissing(err) {
			a.reportStorageError(w)
		} else {
			a.reportError(w, err)
		}
		return
	}
	if full {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	} else {
		writeJSON(w, http.StatusOK, item)
	}
}

func (a *adminAPI) machineStatus(w http.ResponseWriter, r *http.Request, id string) {
	if !reportReadRequest(w, r) {
		return
	}
	status, err := a.store.GetMachineStatus(r.Context(), id)
	if err != nil {
		a.reportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *adminAPI) configurationReport(w http.ResponseWriter, r *http.Request, id, name, kind string) {
	if !reportReadRequest(w, r) {
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
		if storageFailure && !reportResourceMissing(err) {
			a.reportStorageError(w)
		} else {
			a.reportError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func reportResourceMissing(err error) bool {
	return errors.Is(err, database.ErrNotFound) || errors.Is(err, database.ErrConfigurationNotFound) ||
		errors.Is(err, database.ErrReportNotFound)
}

func reportReadRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return false
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "Report retrieval accepts no query parameters")
		return false
	}
	return true
}

func (a *adminAPI) reportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, database.ErrUploadConflict):
		writeError(w, http.StatusConflict, "upload_id_conflict", "Upload ID already identifies a different report")
	case errors.Is(err, database.ErrReportCursor):
		writeError(w, http.StatusBadRequest, "invalid_pagination", "Report cursor does not belong to this machine")
	case errors.Is(err, database.ErrReportNotFound):
		writeError(w, http.StatusNotFound, "report_not_found", "No matching report has been received")
	default:
		a.configurationError(w, err)
	}
}

func (a *adminAPI) reportStorageError(w http.ResponseWriter) {
	a.logger.Error("report storage operation failed")
	writeError(w, http.StatusServiceUnavailable, "report_storage_unavailable", "Report storage operation unavailable")
}
