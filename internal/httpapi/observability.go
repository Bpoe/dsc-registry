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
		return route
	}
	return "unmatched"
}
