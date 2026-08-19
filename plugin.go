package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginID       = "glm-vision-bridge"
	pluginName     = "GLM Vision Bridge"
	repositoryURL  = "https://github.com/wangwq7/cpa-glm-vision-bridge"
	defaultVersion = "1.1.3"
)

var version = defaultVersion
var configured atomic.Value
var telemetry = newEventStore(100)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}
type capabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	ManagementAPI         bool     `json:"management_api"`
}
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}
type rpcRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type hostModelRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload"`
}
type streamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func handleMethod(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(payload); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginID, Models: bridgeModels(currentConfig())})
	case pluginabi.MethodModelRoute:
		return routeModel(payload)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodExecutorExecute:
		return execute(payload)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(payload)
	case pluginabi.MethodExecutorCountTokens:
		return countTokens(payload)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return managementHandle(payload)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, false), nil
	}
}

func configure(raw []byte) error {
	cfg := defaultPluginConfig()
	if len(raw) > 0 {
		var request lifecycleRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		if len(request.ConfigYAML) > 0 {
			if err := yaml.Unmarshal(request.ConfigYAML, &cfg); err != nil {
				return err
			}
		}
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return err
	}
	telemetry.setLimit(normalized.EventLogMaxEntries)
	var cache *memoCache
	if previous := configured.Load(); previous != nil {
		old := previous.(runtimeConfig).cache
		if old.compatible(normalized.CachePath) {
			cache = old
			cache.setLimit(normalized.CacheMaxEntries)
		} else {
			old.close()
		}
	}
	if cache == nil {
		cache = newMemoCache(normalized.CacheMaxEntries, normalized.CachePath)
	}
	configured.Store(runtimeConfig{pluginConfig: normalized, cache: cache, events: telemetry})
	return nil
}
func currentConfig() runtimeConfig {
	if raw := configured.Load(); raw != nil {
		return raw.(runtimeConfig)
	}
	cfg, _ := normalizeConfig(defaultPluginConfig())
	r := runtimeConfig{pluginConfig: cfg, cache: newMemoCache(cfg.CacheMaxEntries, cfg.CachePath), events: telemetry}
	configured.Store(r)
	return r
}
func metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             pluginName,
		Version:          version,
		Author:           "wangwq7",
		GitHubRepository: repositoryURL,
		ConfigFields: []pluginapi.ConfigField{
			{Name: "public_model", Type: pluginapi.ConfigFieldTypeString, Description: "对外暴露的唯一虚拟模型名。"},
			{Name: "primary_model", Type: pluginapi.ConfigFieldTypeString, Description: "最终回答始终优先使用的文本模型。"},
			{Name: "primary_context_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "主文本模型理论上下文上限。"},
			{Name: "primary_context_budget_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "主模型实际输入工作预算，必须低于理论上限。"},
			{Name: "primary_output_token_limit", Type: pluginapi.ConfigFieldTypeInteger, Description: "最终文本输出上限；保留客户端较小值，缺失或过大时按此值注入或封顶。"},
			{Name: "text_fallback_models", Type: pluginapi.ConfigFieldTypeArray, Description: "主文本模型失败且尚未输出内容时依次尝试的备用模型。"},
			{Name: "vision_models", Type: pluginapi.ConfigFieldTypeArray, Description: "按顺序尝试的视觉模型；每项独立设置上下文上限、输入预算、超时和启停状态。"},
			{Name: "vision_prompt", Type: pluginapi.ConfigFieldTypeString, Description: "视觉预处理提示词；图片内容始终按不可信数据处理。"},
			{Name: "vision_input_token_budget", Type: pluginapi.ConfigFieldTypeInteger, Description: "视觉请求携带当前问题附近文字的输入预算；不是输出 token 上限。"},
			{Name: "vision_image_token_reserve", Type: pluginapi.ConfigFieldTypeInteger, Description: "单张图片在视觉上下文预检中的 token 预留量。"},
			{Name: "vision_cancel_grace_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "仅在 stream_close 后等待 Host 确认流结束；未确认时不启动备用模型，不增加正常请求延迟。"},
			{Name: "cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "视觉记忆和历史摘要的持久缓存时长。"},
			{Name: "cache_max_entries", Type: pluginapi.ConfigFieldTypeInteger, Description: "持久缓存最大条数，使用 LRU 淘汰。"},
			{Name: "cache_path", Type: pluginapi.ConfigFieldTypeString, Description: "视觉记忆和历史摘要的持久缓存路径。"},
			{Name: "event_log_max_entries", Type: pluginapi.ConfigFieldTypeInteger, Description: "内存事件日志保留条数，不保存原图。"},
			{Name: "on_vision_failure", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"error", "text_only"}, Description: "所有视觉模型失败时的处理策略。"},
			{Name: "max_images_per_request", Type: pluginapi.ConfigFieldTypeInteger, Description: "当前轮允许完整识别的最大图片数。"},
			{Name: "max_concurrent_extractions", Type: pluginapi.ConfigFieldTypeInteger, Description: "多张图片的并发识别数。"},
			{Name: "max_image_data_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "data URL 图片解码后的真实最大字节数。"},
			{Name: "allow_remote_image_urls", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否允许读取 http/https 图片 URL。"},
			{Name: "history_attachment_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"onDemand", "retain"}, Description: "历史图片按需恢复或完整保留。"},
			{Name: "history_attachment_compact_chars", Type: pluginapi.ConfigFieldTypeInteger, Description: "无关轮中的历史图片归档标记最大字符数。"},
			{Name: "history_attachment_restore_max_attachments", Type: pluginapi.ConfigFieldTypeInteger, Description: "明确引用图片时最多恢复的历史图片数。"},
			{Name: "auto_compression_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "达到阈值后建立可复用的历史摘要检查点。"},
			{Name: "auto_compression_threshold_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "自动压缩触发阈值，必须低于输入工作预算。"},
			{Name: "auto_compression_target_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "历史摘要检查点目标大小；摘要请求会按目标增加容差并显式设置输出预算。"},
			{Name: "auto_compression_keep_recent_turns", Type: pluginapi.ConfigFieldTypeInteger, Description: "优先保留原文的最近语义单元数量；完整工具事务不会拆分。"},
			{Name: "auto_compression_model", Type: pluginapi.ConfigFieldTypeString, Description: "压缩模型；留空使用首选文本模型。"},
		},
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      metadata(),
		Capabilities: capabilities{
			ModelProvider:         true,
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai", "openai-response", "claude"},
			ExecutorOutputFormats: []string{"openai", "openai-response", "claude"},
			ManagementAPI:         true,
		},
	}
}

func bridgeModels(cfg runtimeConfig) []pluginapi.ModelInfo {
	name := strings.TrimSpace(cfg.PublicModel)
	if name == "" {
		return nil
	}
	return []pluginapi.ModelInfo{{
		ID:                         name,
		Object:                     "model",
		OwnedBy:                    pluginID,
		Type:                       "chat",
		DisplayName:                pluginName,
		Description:                "视觉模型只负责转写；最终任务始终由首选文本模型及其文本备用链完成。",
		InputTokenLimit:            int64(cfg.PrimaryContextBudgetTokens),
		OutputTokenLimit:           int64(cfg.PrimaryOutputTokenLimit),
		ContextLength:              int64(cfg.PrimaryContextTokens),
		MaxCompletionTokens:        int64(cfg.PrimaryOutputTokenLimit),
		SupportedGenerationMethods: []string{"chat"},
		SupportedInputModalities:   []string{"text", "image"},
		SupportedOutputModalities:  []string{"text"},
		UserDefined:                true,
	}}
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := currentConfig()
	requested := strings.TrimSpace(req.RequestedModel)
	// Route only the configured public model name.
	matched := requested != "" && requested == strings.TrimSpace(cfg.PublicModel)
	if !cfg.Enabled || !matched {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if _, err := resolveProtocolAdapter(req.SourceFormat, "", req.Body); err != nil {
		// Unsupported or ambiguous protocols must remain available to other
		// routers instead of turning model discovery into a hard failure.
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetSelf, Reason: "glm_vision_bridge_orchestration"})
}

func executorProtocol(req rpcExecutorRequest) (string, error) {
	body := req.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	adapter, err := resolveProtocolAdapter(req.SourceFormat, req.Format, body)
	if err != nil {
		return "", err
	}
	return adapter.protocol, nil
}
