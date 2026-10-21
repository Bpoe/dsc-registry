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

func routeLabel(pattern string) string {
	if _, route, ok := strings.Cut(pattern, " "); ok {
		pattern = route
	}
	switch pattern {
	case "", "/", "/v1/admins", "/v1/admins/{id}", "/v1/agent":
		return "unmatched"
	}
	if strings.HasSuffix(pattern, "/{rest...}") || strings.HasSuffix(pattern, "/{$}") {
		return "unmatched"
	}
	return pattern
}
