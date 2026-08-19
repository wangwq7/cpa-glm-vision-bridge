package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	cpaConfigPath            = "/CLIProxyAPI/config.yaml"
	modelCatalogCacheTTL     = 15 * time.Second
	modelCatalogTimeout      = 2 * time.Second
	defaultCPAManagementPort = 8317
)

//go:embed web/management.html
var managementTemplate string

var modelCatalogCache struct {
	sync.Mutex
	models    []string
	expiresAt time.Time
}

type dashboardConfig struct {
	Version                        string        `json:"version"`
	Enabled                        bool          `json:"enabled"`
	PublicModel                    string        `json:"public_model"`
	PrimaryModel                   string        `json:"primary_model"`
	PrimaryContextTokens           int           `json:"primary_context_tokens"`
	PrimaryContextBudgetTokens     int           `json:"primary_context_budget_tokens"`
	PrimaryOutputTokenLimit        int           `json:"primary_output_token_limit"`
	TextFallbackModels             []string      `json:"text_fallback_models"`
	VisionModels                   []visionModel `json:"vision_models"`
	VisionPrompt                   string        `json:"vision_prompt"`
	VisionInputTokenBudget         int           `json:"vision_input_token_budget"`
	VisionImageTokenReserve        int           `json:"vision_image_token_reserve"`
	VisionCancelGraceSeconds       int           `json:"vision_cancel_grace_seconds"`
	CacheTTLSeconds                int           `json:"cache_ttl_seconds"`
	CacheMaxEntries                int           `json:"cache_max_entries"`
	CachePath                      string        `json:"cache_path"`
	EventLogMaxEntries             int           `json:"event_log_max_entries"`
	OnVisionFailure                string        `json:"on_vision_failure"`
	MaxImagesPerRequest            int           `json:"max_images_per_request"`
	MaxConcurrentExtractions       int           `json:"max_concurrent_extractions"`
	MaxImageDataBytes              int           `json:"max_image_data_bytes"`
	AllowRemoteImageURLs           bool          `json:"allow_remote_image_urls"`
	HistoryAttachmentMode          string        `json:"history_attachment_mode"`
	HistoryAttachmentCompactChars  int           `json:"history_attachment_compact_chars"`
	HistoryRestoreMaxAttachments   int           `json:"history_attachment_restore_max_attachments"`
	AutoCompressionEnabled         bool          `json:"auto_compression_enabled"`
	AutoCompressionThresholdTokens int           `json:"auto_compression_threshold_tokens"`
	AutoCompressionTargetTokens    int           `json:"auto_compression_target_tokens"`
	AutoCompressionKeepRecentTurns int           `json:"auto_compression_keep_recent_turns"`
	AutoCompressionModel           string        `json:"auto_compression_model"`
}

func dashboardConfigFrom(cfg runtimeConfig) dashboardConfig {
	return dashboardConfig{
		Version:                        version,
		Enabled:                        cfg.Enabled,
		PublicModel:                    cfg.PublicModel,
		PrimaryModel:                   cfg.PrimaryModel,
		PrimaryContextTokens:           cfg.PrimaryContextTokens,
		PrimaryContextBudgetTokens:     cfg.PrimaryContextBudgetTokens,
		PrimaryOutputTokenLimit:        cfg.PrimaryOutputTokenLimit,
		TextFallbackModels:             append([]string(nil), cfg.TextFallbackModels...),
		VisionModels:                   append([]visionModel(nil), cfg.VisionModels...),
		VisionPrompt:                   cfg.VisionPrompt,
		VisionInputTokenBudget:         cfg.VisionInputTokenBudget,
		VisionImageTokenReserve:        cfg.VisionImageTokenReserve,
		VisionCancelGraceSeconds:       cfg.VisionCancelGraceSeconds,
		CacheTTLSeconds:                cfg.CacheTTLSeconds,
		CacheMaxEntries:                cfg.CacheMaxEntries,
		CachePath:                      cfg.CachePath,
		EventLogMaxEntries:             cfg.EventLogMaxEntries,
		OnVisionFailure:                cfg.OnVisionFailure,
		MaxImagesPerRequest:            cfg.MaxImagesPerRequest,
		MaxConcurrentExtractions:       cfg.MaxConcurrentExtractions,
		MaxImageDataBytes:              cfg.MaxImageDataBytes,
		AllowRemoteImageURLs:           cfg.AllowRemoteImageURLs,
		HistoryAttachmentMode:          cfg.HistoryAttachmentMode,
		HistoryAttachmentCompactChars:  cfg.HistoryAttachmentCompactChars,
		HistoryRestoreMaxAttachments:   cfg.HistoryRestoreMaxAttachments,
		AutoCompressionEnabled:         cfg.AutoCompressionEnabled,
		AutoCompressionThresholdTokens: cfg.AutoCompressionThresholdTokens,
		AutoCompressionTargetTokens:    cfg.AutoCompressionTargetTokens,
		AutoCompressionKeepRecentTurns: cfg.AutoCompressionKeepRecentTurns,
		AutoCompressionModel:           cfg.AutoCompressionModel,
	}
}

func managementJSONResponse(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	})
}

func currentModelCatalog(cfg runtimeConfig) []string {
	raw, err := os.ReadFile(cpaConfigPath)
	if err != nil {
		return nil
	}
	models := runtimeCPAModels(raw)
	filtered := models[:0]
	for _, model := range models {
		if model != cfg.PublicModel {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func runtimeCPAModels(configYAML []byte) []string {
	now := time.Now()
	modelCatalogCache.Lock()
	if now.Before(modelCatalogCache.expiresAt) {
		models := append([]string(nil), modelCatalogCache.models...)
		modelCatalogCache.Unlock()
		return models
	}
	modelCatalogCache.Unlock()

	port, apiKey := cpaLocalAPISettings(configYAML)
	if apiKey == "" {
		return nil
	}
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/models", port), nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := (&http.Client{Timeout: modelCatalogTimeout}).Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(response.Body).Decode(&payload) != nil {
		return nil
	}
	unique := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		if model := strings.TrimSpace(item.ID); model != "" {
			unique[model] = struct{}{}
		}
	}
	models := make([]string, 0, len(unique))
	for model := range unique {
		models = append(models, model)
	}
	sort.Strings(models)

	modelCatalogCache.Lock()
	modelCatalogCache.models = append([]string(nil), models...)
	modelCatalogCache.expiresAt = now.Add(modelCatalogCacheTTL)
	modelCatalogCache.Unlock()
	return models
}

func cpaLocalAPISettings(configYAML []byte) (int, string) {
	var root map[string]any
	if yaml.Unmarshal(configYAML, &root) != nil {
		return defaultCPAManagementPort, ""
	}
	port := defaultCPAManagementPort
	switch value := root["port"].(type) {
	case int:
		if value > 0 && value < 65_536 {
			port = value
		}
	case int64:
		if value > 0 && value < 65_536 {
			port = int(value)
		}
	case float64:
		if value > 0 && value < 65_536 {
			port = int(value)
		}
	}
	keys, _ := root["api-keys"].([]any)
	for _, value := range keys {
		if key, ok := value.(string); ok && strings.TrimSpace(key) != "" {
			return port, strings.TrimSpace(key)
		}
	}
	return port, ""
}

func managementHTML() string {
	return managementTemplate
}
