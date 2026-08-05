package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type visualAsset struct {
	ID        string
	URL       string
	Path      []string
	ItemIndex int
	Role      string
	Context   string
}

type visualTransformPlan struct {
	CurrentImages    int
	HistoricalImages int
	RestoredImages   int
	ArchivedImages   int
}

func transformOpenAIRequest(raw []byte, cfg runtimeConfig, describe func(visualAsset, string) (string, error)) ([]byte, int, error) {
	return transformRequest(raw, "openai", cfg, describe)
}

func transformRequest(raw []byte, protocol string, cfg runtimeConfig, describe func(visualAsset, string) (string, error)) ([]byte, int, error) {
	return transformRequestWithPlan(raw, protocol, cfg, describe, nil)
}

func transformRequestWithPlan(raw []byte, protocol string, cfg runtimeConfig, describe func(visualAsset, string) (string, error), reportPlan func(visualTransformPlan)) ([]byte, int, error) {
	return transformRequestWithPlanAndMediaHint(raw, protocol, cfg, nil, describe, reportPlan)
}

func transformRequestWithPlanAndMediaHint(raw []byte, protocol string, cfg runtimeConfig, mediaHint *bool, describe func(visualAsset, string) (string, error), reportPlan func(visualTransformPlan)) ([]byte, int, error) {
	adapter, err := adapterForProtocol(protocol)
	if err != nil {
		return nil, 0, err
	}
	mayContainMedia := false
	if mediaHint != nil {
		mayContainMedia = *mediaHint
	} else {
		var valid bool
		mayContainMedia, valid = requestMayContainMedia(raw)
		if !valid {
			return nil, 0, fmt.Errorf("invalid %s request JSON", protocol)
		}
	}
	if !mayContainMedia {
		return raw, 0, nil
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, 0, fmt.Errorf("invalid %s request JSON: %w", adapter.protocol, err)
	}
	assets, mediaIssues := inspectVisualMedia(root, adapter)
	if len(mediaIssues) > 0 {
		return nil, len(assets), fmt.Errorf("unsupported media at %s: %s", strings.Join(mediaIssues[0].Path, "/"), mediaIssues[0].Reason)
	}
	if len(assets) == 0 {
		return raw, 0, nil
	}
	latestIndex, latestText := latestUserTurn(root, adapter)
	for _, asset := range assets {
		if asset.ItemIndex > latestIndex {
			latestIndex = asset.ItemIndex
		}
	}
	current := make([]visualAsset, 0)
	historical := make([]visualAsset, 0)
	for _, asset := range assets {
		if asset.ItemIndex == latestIndex {
			current = append(current, asset)
		} else {
			historical = append(historical, asset)
		}
	}
	if len(current) > cfg.MaxImagesPerRequest {
		return nil, len(assets), fmt.Errorf("current turn contains %d images; maximum is %d", len(current), cfg.MaxImagesPerRequest)
	}
	full := map[string]bool{}
	for _, asset := range current {
		full[asset.ID] = true
	}
	if cfg.HistoryAttachmentMode == "retain" {
		if len(assets) > cfg.MaxImagesPerRequest {
			return nil, len(assets), fmt.Errorf("request contains %d images; maximum is %d in retain mode", len(assets), cfg.MaxImagesPerRequest)
		}
		for _, asset := range historical {
			full[asset.ID] = true
		}
	} else if restoreCount := historicalImageRestoreCount(latestText, cfg.HistoryRestoreMaxAttachments); restoreCount > 0 {
		slots := cfg.MaxImagesPerRequest - len(current)
		count := minInt(restoreCount, slots)
		start := len(historical) - count
		if start < 0 {
			start = 0
		}
		for _, asset := range historical[start:] {
			full[asset.ID] = true
		}
	}
	for index := range assets {
		if full[assets[index].ID] {
			assets[index].Context = trimToTokens(nearbyUserTask(root, assets[index], adapter), cfg.VisionInputTokenBudget)
		}
	}
	if reportPlan != nil {
		restored := 0
		for _, asset := range historical {
			if full[asset.ID] {
				restored++
			}
		}
		reportPlan(visualTransformPlan{
			CurrentImages:    len(current),
			HistoricalImages: len(historical),
			RestoredImages:   restored,
			ArchivedImages:   len(historical) - restored,
		})
	}

	descriptions := make(map[string]string, len(assets))
	archived := make([]visualAsset, 0, len(historical))
	for _, asset := range assets {
		if !full[asset.ID] {
			archived = append(archived, asset)
		}
	}
	if len(archived) > 0 {
		descriptions[archived[0].ID] = archivedVisualMarker(cfg.HistoryAttachmentCompactChars)
		for _, asset := range archived[1:] {
			descriptions[asset.ID] = "[旧图已归档]"
		}
	}

	toResolve := make([]visualAsset, 0)
	for _, asset := range assets {
		if full[asset.ID] {
			toResolve = append(toResolve, asset)
		}
	}
	// Validate every image that will reach the visual chain before starting any
	// upstream call. Archived history is represented by metadata only, so it is
	// never base64-decoded or content-hashed on unrelated text turns.
	for _, asset := range toResolve {
		if err := validateAsset(asset.URL, cfg); err != nil {
			return nil, len(assets), err
		}
	}
	for _, asset := range archived {
		if err := validateArchivedAssetMetadata(asset.URL, cfg); err != nil {
			return nil, len(assets), err
		}
	}
	if len(toResolve) == 0 {
		for _, asset := range assets {
			if !replaceAsset(root, asset.Path, descriptions[asset.ID], adapter) {
				return nil, len(assets), fmt.Errorf("failed to replace media at %s", strings.Join(asset.Path, "/"))
			}
		}
		return finishVisualTransform(root, len(assets), adapter)
	}
	workers := minInt(cfg.MaxConcurrentExtractions, len(toResolve))
	if workers < 1 {
		workers = 1
	}
	type result struct {
		id, description string
		err             error
	}
	jobs := make(chan visualAsset)
	results := make(chan result, len(toResolve))
	for worker := 0; worker < workers; worker++ {
		go func() {
			for asset := range jobs {
				description, err := describe(asset, asset.Context)
				results <- result{id: asset.ID, description: description, err: err}
			}
		}()
	}
	go func() {
		for _, asset := range toResolve {
			jobs <- asset
		}
		close(jobs)
	}()
	resolvedDescriptions := make(map[string]string, len(toResolve))
	for range toResolve {
		item := <-results
		if item.err != nil {
			if cfg.OnVisionFailure != "text_only" {
				return nil, len(assets), item.err
			}
			item.description = "视觉输入未能识别；只能依据本轮文字继续，禁止猜测图片内容。"
		}
		resolvedDescriptions[item.id] = item.description
	}
	for index, asset := range toResolve {
		descriptions[asset.ID] = fullVisualMemory(resolvedDescriptions[asset.ID], index == 0)
	}
	for _, asset := range assets {
		if !replaceAsset(root, asset.Path, descriptions[asset.ID], adapter) {
			return nil, len(assets), fmt.Errorf("failed to replace media at %s", strings.Join(asset.Path, "/"))
		}
	}
	return finishVisualTransform(root, len(assets), adapter)
}

func finishVisualTransform(root any, imageCount int, adapter protocolAdapter) ([]byte, int, error) {
	remaining, residual := inspectVisualMedia(root, adapter)
	if len(residual) > 0 {
		return nil, imageCount, fmt.Errorf("media remained after preprocessing at %s: %s", strings.Join(residual[0].Path, "/"), residual[0].Reason)
	}
	if len(remaining) > 0 {
		return nil, imageCount, fmt.Errorf("%d image attachment(s) remained after preprocessing", len(remaining))
	}
	resultBody, err := json.Marshal(root)
	return resultBody, imageCount, err
}
