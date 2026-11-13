package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/service"
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
	publication, err := a.service.PublishReport(r.Context(), id, body)
	if err != nil {
		a.reportError(r.Context(), w, err)
		return
	}
	w.Header().Set("Location", "/v1/machines/"+id+"/reports/"+publication.Report.ID)
	status := http.StatusOK
	if publication.Outcome != database.MutationUnchanged {
		status = http.StatusCreated
	}
	writeJSON(w, status, publication.Report)
}

func (a *adminAPI) machineReports(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pagination(w, r, validUUID)
	if !ok {
		return
	}
	items, err := a.service.ListReports(r.Context(), r.PathValue("id"), cursor, limit+1)
	if err != nil {
		a.reportError(r.Context(), w, err)
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
	item, content, err := a.service.GetReport(r.Context(), r.PathValue("id"), r.PathValue("reportId"), full)
	if err != nil {
		a.reportError(r.Context(), w, err)
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
	status, err := a.service.GetMachineStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		a.reportError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *adminAPI) configurationStatus(w http.ResponseWriter, r *http.Request) {
	if !reportReadRequest(w, r) {
		return
	}
	status, err := a.service.GetConfigurationStatus(r.Context(), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		a.reportError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *adminAPI) configurationReport(w http.ResponseWriter, r *http.Request) {
	if !reportReadRequest(w, r) {
		return
	}
	content, err := a.service.ReadReport(r.Context(), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		a.reportError(r.Context(), w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func reportReadRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "Report retrieval accepts no query parameters")
		return false
	}
	return true
}

func (a *adminAPI) reportError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, report.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_report", "Expected a valid wrapped dscd v1 report")
	case errors.Is(err, service.ErrReportStorage):
		a.reportStorageError(ctx, w)
	case errors.Is(err, database.ErrUploadConflict):
		writeError(w, http.StatusConflict, "upload_id_conflict", "Upload ID already identifies a different report")
	case errors.Is(err, database.ErrReportCursor):
		writeError(w, http.StatusBadRequest, "invalid_pagination", "Report cursor does not belong to this machine")
	case errors.Is(err, database.ErrReportNotFound):
		writeError(w, http.StatusNotFound, "report_not_found", "No matching report has been received")
	default:
		a.configurationError(ctx, w, err)
	}
}

func (a *adminAPI) reportStorageError(ctx context.Context, w http.ResponseWriter) {
	a.logger.ErrorContext(ctx, "report storage operation failed")
	writeError(w, http.StatusServiceUnavailable, "report_storage_unavailable", "Report storage operation unavailable")
}
