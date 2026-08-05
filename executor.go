package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	protocol, err := executorProtocol(req)
	if err != nil {
		return nil, err
	}
	cfg := currentConfig()
	event := cfg.events.begin(req.Model, cfg.PrimaryModel, false)
	started := time.Now()
	cfg.events.stage(event, "接收桥接请求", "完成", req.Model, fmt.Sprintf("已识别 %s 请求，开始检查多模态内容。", protocol), started)
	body, images, err := preparePrimaryBody(req.OriginalRequest, protocol, cfg, req.HostCallbackID, event)
	cfg.events.setImageCount(event, images)
	if images == 0 {
		cfg.events.stage(event, "纯文本直达", "完成", cfg.PrimaryModel, "未检测到图片，跳过视觉候选链。", time.Now())
	}
	if err != nil {
		cfg.events.stage(event, "多模态预处理", "失败", "", err.Error(), time.Now())
		cfg.events.finish(event, err)
		return nil, err
	}
	body, err = prepareTextHostBody(body, protocol, cfg, req.HostCallbackID, event)
	if err != nil {
		cfg.events.stage(event, "主上下文预算", "失败", cfg.PrimaryModel, err.Error(), time.Now())
		cfg.events.finish(event, err)
		return nil, err
	}
	primaryStarted := time.Now()
	cfg.events.stage(event, "交给文本模型链", "完成", cfg.PrimaryModel, "请求已完成附件处理与上下文预算检查；图片不会进入文本模型。", primaryStarted)
	response, usedModel, err := executeTextFallback(req.HostCallbackID, cfg, body, protocol, event)
	if err != nil {
		cfg.events.stage(event, "文本模型链返回", "失败", usedModel, err.Error(), primaryStarted)
		cfg.events.finish(event, err)
		return nil, err
	}
	cfg.events.stage(event, "文本模型链返回", "完成", usedModel, "已生成最终非流式回答。", primaryStarted)
	cfg.events.finish(event, nil)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: response.Body, Headers: response.Headers})
}
func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required", false), nil
	}
	protocol, err := executorProtocol(req)
	if err != nil {
		return nil, err
	}
	cfg := currentConfig()
	event := cfg.events.begin(req.Model, cfg.PrimaryModel, true)
	go func() {
		started := time.Now()
		cfg.events.stage(event, "接收桥接请求", "完成", req.Model, fmt.Sprintf("已识别流式 %s 请求，开始检查多模态内容。", protocol), started)
		body, images, err := preparePrimaryBody(req.OriginalRequest, protocol, cfg, req.HostCallbackID, event)
		cfg.events.setImageCount(event, images)
		if images == 0 {
			cfg.events.stage(event, "纯文本直达", "完成", cfg.PrimaryModel, "未检测到图片，跳过视觉候选链。", time.Now())
		}
		if err == nil {
			body, err = prepareTextHostBody(body, protocol, cfg, req.HostCallbackID, event)
		}
		if err == nil {
			primaryStarted := time.Now()
			cfg.events.stage(event, "交给文本模型链", "完成", cfg.PrimaryModel, "请求已完成附件处理与上下文预算检查，开始透传输出流。", primaryStarted)
			var usedModel string
			usedModel, err = forwardTextFallbackStream(req.StreamID, req.HostCallbackID, cfg, body, protocol, event)
			if err != nil {
				cfg.events.stage(event, "文本流结束", "失败", usedModel, err.Error(), primaryStarted)
			} else {
				cfg.events.stage(event, "文本流结束", "完成", usedModel, "流式输出已完整透传。", primaryStarted)
			}
		} else {
			cfg.events.stage(event, "多模态预处理", "失败", "", err.Error(), time.Now())
		}
		cfg.events.finish(event, err)
		closePluginStream(req.StreamID, err)
	}()
	return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

func prepareTextHostBody(raw []byte, protocol string, cfg runtimeConfig, callbackID string, event *bridgeEvent) ([]byte, error) {
	body, err := prepareFinalTextBody(raw, cfg, callbackID, event)
	if err != nil {
		return nil, err
	}
	body, decision, err := applyFinalTextOutputLimit(body, protocol, cfg.PrimaryOutputTokenLimit)
	if err != nil {
		return nil, err
	}
	cfg.events.stage(event, "最终输出预算", "完成", cfg.PrimaryModel, decision.detail(), time.Now())
	return body, nil
}
