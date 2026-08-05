package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type streamTerminationTracker struct {
	pending string
	reason  string
}

func (t *streamTerminationTracker) add(payload []byte) string {
	if t.reason != "" {
		return t.reason
	}
	t.pending += string(payload)
	for {
		index := strings.IndexByte(t.pending, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimSuffix(t.pending[:index], "\r")
		t.pending = t.pending[index+1:]
		if reason := streamLineTruncationReason(line); reason != "" {
			t.reason = reason
			return reason
		}
	}
	if streamLineIsComplete(t.pending) {
		t.reason = streamLineTruncationReason(t.pending)
		t.pending = ""
	}
	return t.reason
}

func streamLineTruncationReason(line string) string {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" || line == "[DONE]" || line == "data: [DONE]" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
		return ""
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	return responseTruncationReason([]byte(line))
}

const maxBufferedTextStreamBytes = 1024 * 1024

type textStreamOutputGate struct {
	pending  string
	payloads [][]byte
	size     int
}

func (g *textStreamOutputGate) add(payload []byte) (bool, [][]byte, error) {
	copyPayload := append([]byte(nil), payload...)
	g.payloads = append(g.payloads, copyPayload)
	g.size += len(copyPayload)
	if g.size > maxBufferedTextStreamBytes {
		return false, nil, fmt.Errorf("text stream exceeded %d buffered metadata bytes before producing deliverable output", maxBufferedTextStreamBytes)
	}
	g.pending += string(payload)
	deliverable := false
	for {
		index := strings.IndexByte(g.pending, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimSuffix(g.pending[:index], "\r")
		g.pending = g.pending[index+1:]
		if streamLineHasDeliverableOutput(line) {
			deliverable = true
		}
	}
	if streamLineIsComplete(g.pending) {
		if streamLineHasDeliverableOutput(g.pending) {
			deliverable = true
		}
		g.pending = ""
	}
	if !deliverable {
		return false, nil, nil
	}
	buffered := g.payloads
	g.payloads = nil
	g.size = 0
	g.pending = ""
	return true, buffered, nil
}

func streamLineIsComplete(line string) bool {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" || line == "[DONE]" || line == "data: [DONE]" {
		return true
	}
	if strings.HasPrefix(line, "event:") && !strings.Contains(line, "{") {
		return true
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	return json.Valid([]byte(line))
}

func streamLineHasDeliverableOutput(line string) bool {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" || line == "[DONE]" || line == "data: [DONE]" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
		return false
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	var root map[string]any
	if json.Unmarshal([]byte(line), &root) != nil {
		return false
	}
	return streamEventHasDeliverableOutput(root)
}

func streamEventHasDeliverableOutput(root map[string]any) bool {
	eventType := strings.ToLower(strings.TrimSpace(stringValue(root["type"])))
	if eventType == "response.output_text.delta" || eventType == "response.refusal.delta" {
		return strings.TrimSpace(stringValue(root["delta"])) != ""
	}
	if strings.Contains(eventType, "reasoning") || strings.Contains(eventType, "thinking") {
		return strings.TrimSpace(stringValue(root["delta"])) != ""
	}
	if eventType == "response.output_text.done" {
		return strings.TrimSpace(stringValue(root["text"])) != ""
	}
	if strings.Contains(eventType, "function_call") || strings.Contains(eventType, "custom_tool_call") || strings.Contains(eventType, "computer_call") {
		return true
	}
	if eventType == "content_block_start" {
		if block, ok := root["content_block"].(map[string]any); ok {
			return outputItemHasDeliverableContent(block)
		}
	}
	if eventType == "content_block_delta" {
		if delta, ok := root["delta"].(map[string]any); ok {
			deltaType := strings.ToLower(strings.TrimSpace(stringValue(delta["type"])))
			if deltaType == "text_delta" {
				return strings.TrimSpace(stringValue(delta["text"])) != ""
			}
			if deltaType == "thinking_delta" {
				return strings.TrimSpace(stringValue(delta["thinking"])) != ""
			}
			return deltaType == "input_json_delta" && strings.TrimSpace(stringValue(delta["partial_json"])) != ""
		}
	}
	if item, ok := root["item"].(map[string]any); ok && outputItemHasDeliverableContent(item) {
		return true
	}
	if choices, ok := root["choices"].([]any); ok {
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			if strings.TrimSpace(contentText(choice["text"])) != "" {
				return true
			}
			for _, key := range []string{"delta", "message"} {
				message, _ := choice[key].(map[string]any)
				if outputItemHasDeliverableContent(message) {
					return true
				}
			}
		}
	}
	return false
}

func hasDeliverableModelResponse(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	if strings.TrimSpace(contentText(root["output_text"])) != "" || strings.TrimSpace(contentText(root["completion"])) != "" {
		return true
	}
	if choices, ok := root["choices"].([]any); ok {
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			if strings.TrimSpace(contentText(choice["text"])) != "" {
				return true
			}
			if message, ok := choice["message"].(map[string]any); ok && outputItemHasDeliverableContent(message) {
				return true
			}
		}
	}
	for _, field := range []string{"output", "content"} {
		if items, ok := root[field].([]any); ok {
			for _, rawItem := range items {
				item, _ := rawItem.(map[string]any)
				if outputItemHasDeliverableContent(item) {
					return true
				}
			}
		}
	}
	return false
}

func outputItemHasDeliverableContent(item map[string]any) bool {
	if len(item) == 0 {
		return false
	}
	for _, key := range []string{"content", "text", "refusal", "reasoning_content", "reasoning", "thinking"} {
		if strings.TrimSpace(contentText(item[key])) != "" {
			return true
		}
	}
	for _, key := range []string{"tool_calls", "function_call"} {
		switch value := item[key].(type) {
		case []any:
			if len(value) > 0 {
				return true
			}
		case map[string]any:
			if len(value) > 0 {
				return true
			}
		}
	}
	itemType := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
	if itemType == "tool_use" || strings.HasSuffix(itemType, "_call") {
		return true
	}
	if content, ok := item["content"].([]any); ok {
		for _, rawPart := range content {
			part, _ := rawPart.(map[string]any)
			if outputItemHasDeliverableContent(part) {
				return true
			}
		}
	}
	return false
}
func closePluginStream(streamID string, err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: message})
}
