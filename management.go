package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}
type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
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
			{Method: http.MethodGet, Path: "/glm-vision-bridge/config"},
			{Method: http.MethodGet, Path: "/glm-vision-bridge/events"},
			{Method: http.MethodGet, Path: "/glm-vision-bridge/model-catalog"},
		},
		Resources: []resourceRoute{{Path: "/open", Menu: "GLM Vision Bridge", Description: "查看桥接事件、视觉处理链路并编辑配置。"}},
	}
}
func managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	switch {
	case strings.HasSuffix(req.Path, "/events"):
		cfg := currentConfig()
		return managementJSONResponse(cfg.events.snapshot())
	case strings.HasSuffix(req.Path, "/model-catalog"):
		cfg := currentConfig()
		return managementJSONResponse(currentModelCatalog(cfg))
	case strings.HasSuffix(req.Path, "/config"):
		cfg := currentConfig()
		return managementJSONResponse(dashboardConfigFrom(cfg))
	default:
		return okEnvelope(managementResponse{StatusCode: 200, Headers: http.Header{"content-type": []string{"text/html; charset=utf-8"}, "cache-control": []string{"no-store"}}, Body: []byte(managementHTML())})
	}
}
