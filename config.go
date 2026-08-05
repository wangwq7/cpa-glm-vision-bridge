package main

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

type visionModel struct {
	Model          string `yaml:"model" json:"model"`
	ContextLimit   int    `yaml:"context_limit" json:"context_limit"`
	ContextBudget  int    `yaml:"context_budget" json:"context_budget"`
	TimeoutSeconds int    `yaml:"timeout_seconds" json:"timeout_seconds"`
	Enabled        *bool  `yaml:"enabled" json:"enabled"`
}

func (model visionModel) active() bool {
	return model.Enabled == nil || *model.Enabled
}

type pluginConfig struct {
	Enabled                        bool          `yaml:"enabled"`
	PublicModel                    string        `yaml:"public_model"`
	PrimaryModel                   string        `yaml:"primary_model"`
	PrimaryContextTokens           int           `yaml:"primary_context_tokens"`
	PrimaryContextBudgetTokens     int           `yaml:"primary_context_budget_tokens"`
	PrimaryOutputTokenLimit        int           `yaml:"primary_output_token_limit"`
	TextFallbackModels             []string      `yaml:"text_fallback_models"`
	VisionModels                   []visionModel `yaml:"vision_models"`
	VisionPrompt                   string        `yaml:"vision_prompt"`
	VisionInputTokenBudget         int           `yaml:"vision_input_token_budget"`
	VisionImageTokenReserve        int           `yaml:"vision_image_token_reserve"`
	VisionCancelGraceSeconds       int           `yaml:"vision_cancel_grace_seconds"`
	CacheTTLSeconds                int           `yaml:"cache_ttl_seconds"`
	CacheMaxEntries                int           `yaml:"cache_max_entries"`
	CachePath                      string        `yaml:"cache_path"`
	EventLogMaxEntries             int           `yaml:"event_log_max_entries"`
	OnVisionFailure                string        `yaml:"on_vision_failure"`
	MaxImagesPerRequest            int           `yaml:"max_images_per_request"`
	MaxConcurrentExtractions       int           `yaml:"max_concurrent_extractions"`
	MaxImageDataBytes              int           `yaml:"max_image_data_bytes"`
	AllowRemoteImageURLs           bool          `yaml:"allow_remote_image_urls"`
	HistoryAttachmentMode          string        `yaml:"history_attachment_mode"`
	HistoryAttachmentCompactChars  int           `yaml:"history_attachment_compact_chars"`
	HistoryRestoreMaxAttachments   int           `yaml:"history_attachment_restore_max_attachments"`
	AutoCompressionEnabled         bool          `yaml:"auto_compression_enabled"`
	AutoCompressionThresholdTokens int           `yaml:"auto_compression_threshold_tokens"`
	AutoCompressionTargetTokens    int           `yaml:"auto_compression_target_tokens"`
	AutoCompressionKeepRecentTurns int           `yaml:"auto_compression_keep_recent_turns"`
	AutoCompressionModel           string        `yaml:"auto_compression_model"`
}

type runtimeConfig struct {
	pluginConfig
	cache             *memoCache
	events            *eventStore
	historySummarizer historySummarizerFunc
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:                    true,
		PublicModel:                "glm-vision-bridge",
		PrimaryModel:               "glm-5.2",
		PrimaryContextTokens:       1_000_000,
		PrimaryContextBudgetTokens: 900_000,
		PrimaryOutputTokenLimit:    64_000,
		TextFallbackModels:         []string{"gpt-5.5", "gpt-5.6-sol"},
		VisionModels: []visionModel{
			{Model: "gemini-3.1-flash-lite", ContextLimit: 262_144, ContextBudget: 180_000, TimeoutSeconds: 20},
			{Model: "gpt-5.6-terra", ContextLimit: 262_144, ContextBudget: 180_000, TimeoutSeconds: 20},
			{Model: "grok-4.5", ContextLimit: 262_144, ContextBudget: 180_000, TimeoutSeconds: 20},
			{Model: "claude-sonnet-4-6", ContextLimit: 262_144, ContextBudget: 180_000, TimeoutSeconds: 20},
		},
		VisionPrompt:                   defaultVisionPrompt,
		VisionInputTokenBudget:         1_200,
		VisionImageTokenReserve:        4_096,
		VisionCancelGraceSeconds:       15,
		CacheTTLSeconds:                72 * 60 * 60,
		CacheMaxEntries:                2_000,
		CachePath:                      "/CLIProxyAPI/plugins/data/glm-vision-bridge-cache.json",
		EventLogMaxEntries:             100,
		OnVisionFailure:                "error",
		MaxImagesPerRequest:            8,
		MaxConcurrentExtractions:       2,
		MaxImageDataBytes:              12 * 1024 * 1024,
		AllowRemoteImageURLs:           true,
		HistoryAttachmentMode:          "onDemand",
		HistoryAttachmentCompactChars:  600,
		HistoryRestoreMaxAttachments:   2,
		AutoCompressionEnabled:         true,
		AutoCompressionThresholdTokens: 820_000,
		AutoCompressionTargetTokens:    12_000,
		AutoCompressionKeepRecentTurns: 8,
	}
}

func normalizeConfig(cfg pluginConfig) (pluginConfig, error) {
	defaults := defaultPluginConfig()
	defaultString := func(value *string, fallback string) {
		if strings.TrimSpace(*value) == "" {
			*value = fallback
		}
		*value = strings.TrimSpace(*value)
	}
	defaultInt := func(value *int, fallback int) {
		if *value <= 0 {
			*value = fallback
		}
	}

	defaultString(&cfg.PublicModel, defaults.PublicModel)
	defaultString(&cfg.PrimaryModel, defaults.PrimaryModel)
	defaultString(&cfg.VisionPrompt, defaults.VisionPrompt)
	defaultString(&cfg.CachePath, defaults.CachePath)
	defaultInt(&cfg.PrimaryContextTokens, defaults.PrimaryContextTokens)
	defaultInt(&cfg.PrimaryContextBudgetTokens, defaults.PrimaryContextBudgetTokens)
	if cfg.PrimaryOutputTokenLimit <= 0 {
		cfg.PrimaryOutputTokenLimit = minInt(defaults.PrimaryOutputTokenLimit, cfg.PrimaryContextTokens-cfg.PrimaryContextBudgetTokens)
	}
	defaultInt(&cfg.VisionInputTokenBudget, defaults.VisionInputTokenBudget)
	defaultInt(&cfg.VisionImageTokenReserve, defaults.VisionImageTokenReserve)
	defaultInt(&cfg.VisionCancelGraceSeconds, defaults.VisionCancelGraceSeconds)
	defaultInt(&cfg.CacheTTLSeconds, defaults.CacheTTLSeconds)
	defaultInt(&cfg.CacheMaxEntries, defaults.CacheMaxEntries)
	defaultInt(&cfg.EventLogMaxEntries, defaults.EventLogMaxEntries)
	defaultInt(&cfg.MaxImagesPerRequest, defaults.MaxImagesPerRequest)
	defaultInt(&cfg.MaxConcurrentExtractions, defaults.MaxConcurrentExtractions)
	defaultInt(&cfg.MaxImageDataBytes, defaults.MaxImageDataBytes)
	defaultInt(&cfg.HistoryAttachmentCompactChars, defaults.HistoryAttachmentCompactChars)
	defaultInt(&cfg.HistoryRestoreMaxAttachments, defaults.HistoryRestoreMaxAttachments)
	defaultInt(&cfg.AutoCompressionThresholdTokens, defaults.AutoCompressionThresholdTokens)
	defaultInt(&cfg.AutoCompressionTargetTokens, defaults.AutoCompressionTargetTokens)
	defaultInt(&cfg.AutoCompressionKeepRecentTurns, defaults.AutoCompressionKeepRecentTurns)

	if cfg.PrimaryContextBudgetTokens >= cfg.PrimaryContextTokens {
		return cfg, fmt.Errorf("primary_context_budget_tokens must be lower than primary_context_tokens")
	}
	if cfg.PrimaryOutputTokenLimit > cfg.PrimaryContextTokens-cfg.PrimaryContextBudgetTokens {
		return cfg, fmt.Errorf("primary_output_token_limit must fit between primary_context_budget_tokens and primary_context_tokens")
	}
	if cfg.AutoCompressionThresholdTokens >= cfg.PrimaryContextBudgetTokens {
		return cfg, fmt.Errorf("auto_compression_threshold_tokens must be lower than primary_context_budget_tokens")
	}
	if cfg.AutoCompressionTargetTokens >= cfg.AutoCompressionThresholdTokens {
		return cfg, fmt.Errorf("auto_compression_target_tokens must be lower than auto_compression_threshold_tokens")
	}
	if cfg.PrimaryModel == cfg.PublicModel {
		return cfg, fmt.Errorf("primary model %s cannot point back to the bridge", cfg.PrimaryModel)
	}

	cfg.TextFallbackModels = uniqueModels(cfg.TextFallbackModels, cfg.PrimaryModel)
	for _, model := range cfg.TextFallbackModels {
		if model == cfg.PublicModel {
			return cfg, fmt.Errorf("text fallback model %s cannot point back to the bridge", model)
		}
	}

	cfg.AutoCompressionModel = strings.TrimSpace(cfg.AutoCompressionModel)
	if cfg.AutoCompressionModel == cfg.PublicModel {
		return cfg, fmt.Errorf("auto compression model %s cannot point back to the bridge", cfg.AutoCompressionModel)
	}
	cfg.HistoryAttachmentMode = strings.TrimSpace(cfg.HistoryAttachmentMode)
	if cfg.HistoryAttachmentMode == "" {
		cfg.HistoryAttachmentMode = defaults.HistoryAttachmentMode
	}
	if cfg.HistoryAttachmentMode != "retain" && cfg.HistoryAttachmentMode != "onDemand" {
		return cfg, fmt.Errorf("history_attachment_mode must be retain or onDemand")
	}

	cfg.OnVisionFailure = strings.ToLower(strings.TrimSpace(cfg.OnVisionFailure))
	if cfg.OnVisionFailure == "" {
		cfg.OnVisionFailure = defaults.OnVisionFailure
	}
	if cfg.OnVisionFailure != "error" && cfg.OnVisionFailure != "text_only" {
		return cfg, fmt.Errorf("on_vision_failure must be error or text_only")
	}

	if cfg.MaxConcurrentExtractions > 8 {
		cfg.MaxConcurrentExtractions = 8
	}
	if cfg.VisionCancelGraceSeconds > 120 {
		cfg.VisionCancelGraceSeconds = 120
	}
	if cfg.HistoryRestoreMaxAttachments > 16 {
		cfg.HistoryRestoreMaxAttachments = 16
	}
	if cfg.HistoryAttachmentCompactChars < 120 {
		cfg.HistoryAttachmentCompactChars = 120
	}
	if cfg.HistoryAttachmentCompactChars > 4_000 {
		cfg.HistoryAttachmentCompactChars = 4_000
	}

	models, err := normalizeVisionModels(cfg, defaults)
	if err != nil {
		return cfg, err
	}
	cfg.VisionModels = models
	return cfg, nil
}

func normalizeVisionModels(cfg pluginConfig, defaults pluginConfig) ([]visionModel, error) {
	models := cfg.VisionModels
	if models == nil {
		models = defaults.VisionModels
	} else if len(models) == 0 {
		return nil, fmt.Errorf("at least one visual model is required")
	}
	if len(models) > 4 {
		return nil, fmt.Errorf("at most four visual models are supported")
	}

	textModels := map[string]bool{cfg.PrimaryModel: true}
	for _, model := range cfg.TextFallbackModels {
		textModels[model] = true
	}
	if cfg.AutoCompressionModel != "" {
		textModels[cfg.AutoCompressionModel] = true
	}
	seen := make(map[string]bool, len(models))
	clean := make([]visionModel, 0, len(models))
	active := 0
	for _, candidate := range models {
		candidate.Model = strings.TrimSpace(candidate.Model)
		if candidate.Model == "" {
			continue
		}
		if candidate.Model == cfg.PublicModel {
			return nil, fmt.Errorf("visual model %s cannot point back to the bridge", candidate.Model)
		}
		if textModels[candidate.Model] {
			return nil, fmt.Errorf("model %s cannot be used in both text and visual chains", candidate.Model)
		}
		if seen[candidate.Model] {
			return nil, fmt.Errorf("visual model %s is duplicated", candidate.Model)
		}
		seen[candidate.Model] = true

		if candidate.ContextLimit <= 0 {
			candidate.ContextLimit = 262_144
		}
		if candidate.ContextLimit <= 1_024 {
			return nil, fmt.Errorf("visual model %s context_limit must exceed 1024", candidate.Model)
		}
		if candidate.ContextBudget <= 0 {
			candidate.ContextBudget = minInt(180_000, candidate.ContextLimit-8_192)
			if candidate.ContextBudget <= 0 {
				candidate.ContextBudget = candidate.ContextLimit - 1_024
			}
		}
		if candidate.ContextBudget >= candidate.ContextLimit {
			return nil, fmt.Errorf("visual model %s context_budget must be lower than context_limit", candidate.Model)
		}
		if candidate.TimeoutSeconds <= 0 {
			candidate.TimeoutSeconds = 20
		}
		if candidate.active() {
			active++
		}
		clean = append(clean, candidate)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("at least one visual model is required")
	}
	if active == 0 {
		return nil, fmt.Errorf("at least one visual model must be enabled")
	}
	return clean, nil
}

func uniqueModels(values []string, excluded ...string) []string {
	blocked := make(map[string]bool, len(values)+len(excluded))
	for _, item := range excluded {
		blocked[strings.TrimSpace(item)] = true
	}
	models := make([]string, 0, len(values))
	for _, item := range values {
		item = strings.TrimSpace(item)
		if item == "" || blocked[item] {
			continue
		}
		blocked[item] = true
		models = append(models, item)
	}
	return models
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func validateConfigFields(raw []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}

	allowed := map[string]bool{"priority": true, "store": true}
	configType := reflect.TypeOf(pluginConfig{})
	for index := 0; index < configType.NumField(); index++ {
		name := strings.Split(configType.Field(index).Tag.Get("yaml"), ",")[0]
		if name != "" && name != "-" {
			allowed[name] = true
		}
	}
	mapping := root.Content[0]
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		field := strings.TrimSpace(mapping.Content[index].Value)
		if !allowed[field] {
			return fmt.Errorf("unknown 1.0 configuration field %q", field)
		}
		if field == "vision_models" {
			if err := validateVisionModelFields(mapping.Content[index+1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateVisionModelFields(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	allowed := map[string]bool{
		"model": true, "context_limit": true, "context_budget": true,
		"timeout_seconds": true, "enabled": true,
	}
	for modelIndex, candidate := range node.Content {
		if candidate == nil || candidate.Kind != yaml.MappingNode {
			continue
		}
		for index := 0; index+1 < len(candidate.Content); index += 2 {
			field := strings.TrimSpace(candidate.Content[index].Value)
			if !allowed[field] {
				return fmt.Errorf("unknown 1.0 vision_models[%d] field %q", modelIndex, field)
			}
		}
	}
	return nil
}
