package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func countTokens(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	body := req.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	payload, err := json.Marshal(map[string]int{"input_tokens": estimateExecutorInputTokens(body, currentConfig())})
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

func estimateExecutorInputTokens(body []byte, cfg runtimeConfig) int {
	if len(body) == 0 {
		return 0
	}
	var root any
	if json.Unmarshal(body, &root) != nil {
		return estimateBodyTokens(body)
	}
	adapter, err := detectProtocolAdapterFromRoot(root, false)
	if err != nil {
		return estimateBodyTokens(body)
	}
	assets, mediaIssues := inspectVisualMedia(root, adapter)
	if len(mediaIssues) > 0 || len(assets) == 0 {
		return estimateBodyTokens(body)
	}
	userIndex, latestText := latestUserTurn(root, adapter)
	current, historical := splitCurrentVisualBatch(root, adapter, userIndex, assets)
	full := make(map[string]bool, len(assets))
	for _, asset := range current {
		full[asset.ID] = true
	}
	currentCount := len(current)
	if cfg.HistoryAttachmentMode == "retain" {
		for _, asset := range historical {
			full[asset.ID] = true
		}
	} else if restoreCount := historicalImageRestoreCount(latestText, cfg.HistoryRestoreMaxAttachments); restoreCount > 0 {
		slots := cfg.MaxImagesPerRequest - currentCount
		if slots < 0 {
			slots = 0
		}
		count := minInt(restoreCount, slots)
		start := len(historical) - count
		if start < 0 {
			start = 0
		}
		for _, asset := range historical[start:] {
			full[asset.ID] = true
		}
	}
	reserve := cfg.VisionImageTokenReserve
	if reserve <= 0 {
		reserve = defaultPluginConfig().VisionImageTokenReserve
	}
	placeholder := strings.Repeat("x", reserve*3)
	archivedCount := 0
	for _, asset := range assets {
		replacement := placeholder
		if !full[asset.ID] {
			if archivedCount == 0 {
				replacement = archivedVisualMarker(cfg.HistoryAttachmentCompactChars)
			} else {
				replacement = "[旧图已归档]"
			}
			archivedCount++
		}
		if !replaceAsset(root, asset.Path, replacement, adapter) {
			return estimateBodyTokens(body)
		}
	}
	normalized, err := json.Marshal(root)
	if err != nil {
		return estimateBodyTokens(body)
	}
	return estimateBodyTokens(normalized)
}
