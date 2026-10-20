package httpapi

import (
	"net/http"
	"strings"
)

func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodDelete, http.MethodPatch, http.MethodOptions:
		return method
	default:
		return "other"
	}
}

func routeLabel(path string) string {
	if path == "/health/live" || path == "/health/ready" || path == "/v1/agent/configurations" ||
		path == "/v1/agent/reports" || path == "/v1/machines" ||
		path == "/v1/registration-tokens" || path == "/v1/agent/register" {
		return path
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" {
		return "unmatched"
	}
	if parts[1] == "agent" {
		if len(parts) == 5 && parts[2] == "configurations" && parts[4] == "report" {
			return "/v1/agent/configurations/{name}/report"
		}
		return "unmatched"
	}
	if parts[1] != "machines" && parts[1] != "admins" {
		return "unmatched"
	}
	base := "/v1/" + parts[1] + "/{id}"
	if (len(parts) == 4 || len(parts) == 5) && parts[3] == "api-keys" {
		if len(parts) == 5 {
			return base + "/api-keys/{keyId}"
		}
		return base + "/api-keys"
	}
	if parts[1] == "machines" {
		if len(parts) == 3 {
			return base
		}
		if len(parts) == 4 && parts[3] == "status" {
			return base + "/status"
		}
		if parts[3] == "reports" {
			switch len(parts) {
			case 4:
				return base + "/reports"
			case 5:
				return base + "/reports/{reportId}"
			case 6:
				if parts[5] == "content" {
					return base + "/reports/{reportId}/content"
				}
			}
		}
		if parts[3] == "configurations" {
			switch len(parts) {
			case 4:
				return base + "/configurations"
			case 5:
				return base + "/configurations/{name}"
			case 6:
				if parts[5] == "report" || parts[5] == "status" {
					return base + "/configurations/{name}/" + parts[5]
				}
			}
		}
	}
	return "unmatched"
}
