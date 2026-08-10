package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRegistrationResponse struct {
	Routes []managementRoute `json:"routes,omitempty"`
}
type managementRoute struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
}
type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: "/glm-vision-bridge"},
			{Method: http.MethodGet, Path: "/glm-vision-bridge/events"},
			{Method: http.MethodGet, Path: "/glm-vision-bridge/model-catalog"},
		},
	}
}
func managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := currentConfig()
	switch {
	case strings.HasSuffix(req.Path, "/events"):
		return managementJSONResponse(cfg.events.snapshot())
	case strings.HasSuffix(req.Path, "/model-catalog"):
		return managementJSONResponse(currentModelCatalog(cfg))
	default:
		return okEnvelope(managementResponse{StatusCode: 200, Headers: http.Header{"content-type": []string{"text/html; charset=utf-8"}, "cache-control": []string{"no-store"}}, Body: []byte(managementHTML(cfg))})
	}
}
