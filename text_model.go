package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func executeTextFallback(callbackID string, cfg runtimeConfig, body []byte, protocol string, event *bridgeEvent) (pluginapi.HostModelExecutionResponse, string, error) {
	var lastErr error
	models := textModels(cfg)
	for index, model := range models {
		started := time.Now()
		response, err := hostExecuteProtocol(callbackID, model, body, false, protocol)
		if err == nil {
			if reason := responseTruncationReason(response.Body); reason != "" {
				err = fmt.Errorf("text model %s returned truncated output (%s)", model, reason)
			}
		}
		if err == nil {
			return response, model, nil
		}
		lastErr = err
		detail := err.Error()
		if index+1 < len(models) {
			detail += "；尝试下一个文本备用模型。"
		}
		cfg.events.stage(event, "文本候选调用", "失败", model, detail, started)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no text model is configured")
	}
	return pluginapi.HostModelExecutionResponse{}, models[len(models)-1], lastErr
}

func hostExecute(callbackID, model string, body []byte, stream bool) (pluginapi.HostModelExecutionResponse, error) {
	return hostExecuteProtocol(callbackID, model, body, stream, "openai")
}

func hostExecuteProtocol(callbackID, model string, body []byte, stream bool, protocol string) (pluginapi.HostModelExecutionResponse, error) {
	raw, err := callHost(pluginabi.MethodHostModelExecute, makeHostModelRequest(callbackID, protocol, model, body, stream))
	if err != nil {
		return pluginapi.HostModelExecutionResponse{}, err
	}
	var response pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return response, err
	}
	if response.StatusCode >= 400 {
		return response, fmt.Errorf("upstream model %s returned HTTP %d", model, response.StatusCode)
	}
	if !stream && !hasDeliverableModelResponse(response.Body) {
		return response, fmt.Errorf("upstream model %s returned HTTP %d without deliverable output", model, response.StatusCode)
	}
	return response, nil
}

func makeHostModelRequest(callbackID, protocol, model string, body []byte, stream bool) hostModelRequest {
	return hostModelRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: protocol,
			ExitProtocol:  protocol,
			Model:         model,
			Stream:        stream,
			Body:          body,
		},
		HostCallbackID: callbackID,
	}
}

// hostExecuteWithTimeout is retained for the history-compression path. The
// non-streaming Host ABI has no cancellation primitive, so this is only a
// latency annotation and must never be used for visual-model fallback.
func hostExecuteWithTimeout(callbackID, model string, body []byte, seconds int) (pluginapi.HostModelExecutionResponse, error) {
	started := time.Now()
	response, err := hostExecute(callbackID, model, body, false)
	if err != nil && seconds > 0 && time.Since(started) > time.Duration(seconds)*time.Second {
		return response, fmt.Errorf("model %s failed after exceeding the %ds soft latency budget: %w", model, seconds, err)
	}
	return response, err
}

func forwardTextFallbackStream(streamID, callbackID string, cfg runtimeConfig, body []byte, protocol string, event *bridgeEvent) (string, error) {
	models := textModels(cfg)
	var lastErr error
	for index, model := range models {
		started := time.Now()
		emitted, err := forwardPrimaryStream(streamID, callbackID, model, body, protocol)
		if err == nil {
			return model, nil
		}
		lastErr = err
		// Once bytes reached the client, switching models would duplicate or mix
		// two answers in one stream. Fallback is therefore safe only before the
		// first emitted chunk.
		if emitted {
			return model, err
		}
		detail := err.Error()
		if index+1 < len(models) {
			detail += "；尚未输出内容，安全切换到下一个文本备用模型。"
		}
		cfg.events.stage(event, "文本流候选", "失败", model, detail, started)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no text model is configured")
	}
	return models[len(models)-1], lastErr
}

func forwardPrimaryStream(streamID, callbackID, model string, body []byte, protocol string) (bool, error) {
	return forwardPrimaryStreamWithHost(streamID, callbackID, model, body, protocol, callHost)
}

func forwardPrimaryStreamWithHost(streamID, callbackID, model string, body []byte, protocol string, invoke hostCallFunc) (bool, error) {
	raw, err := invoke(pluginabi.MethodHostModelExecuteStream, makeHostModelRequest(callbackID, protocol, model, body, true))
	if err != nil {
		return false, err
	}
	var started pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(raw, &started); err != nil {
		return false, err
	}
	if started.StatusCode >= 400 {
		return false, fmt.Errorf("text model %s returned HTTP %d", model, started.StatusCode)
	}
	if started.StreamID == "" {
		return false, fmt.Errorf("text model %s returned no stream id", model)
	}
	defer func() {
		_, _ = invoke(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: started.StreamID})
	}()
	emitted := false
	gate := textStreamOutputGate{}
	termination := streamTerminationTracker{}
	for {
		chunkRaw, err := invoke(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: started.StreamID})
		if err != nil {
			return emitted, err
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			return emitted, err
		}
		if chunk.Error != "" {
			return emitted, fmt.Errorf("text stream error: %s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			truncationReason := termination.add(chunk.Payload)
			if truncationReason != "" && !emitted {
				return false, fmt.Errorf("text model %s returned truncated output (%s)", model, truncationReason)
			}
			if emitted {
				if _, err = invoke(pluginabi.MethodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: chunk.Payload}); err != nil {
					return emitted, err
				}
				if truncationReason != "" {
					return true, fmt.Errorf("text model %s returned truncated output (%s)", model, truncationReason)
				}
			} else {
				ready, buffered, gateErr := gate.add(chunk.Payload)
				if gateErr != nil {
					return false, gateErr
				}
				if ready {
					for _, payload := range buffered {
						if _, err = invoke(pluginabi.MethodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: payload}); err != nil {
							return emitted, err
						}
						emitted = true
					}
				}
			}
		}
		if chunk.Done {
			if !emitted {
				return false, fmt.Errorf("text model %s completed without deliverable output", model)
			}
			return emitted, nil
		}
	}
}
