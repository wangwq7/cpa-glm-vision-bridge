package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func collectVisualAssets(root any) []visualAsset {
	adapter, err := detectProtocolAdapterFromRoot(root, false)
	if err != nil {
		return nil
	}
	assets, _ := inspectVisualMedia(root, adapter)
	return assets
}

func collectVisualAssetsForProtocol(root any, protocol string) ([]visualAsset, error) {
	adapter, err := adapterForProtocol(protocol)
	if err != nil {
		return nil, err
	}
	assets, issues := inspectVisualMedia(root, adapter)
	if len(issues) > 0 {
		return assets, fmt.Errorf("unsupported media at %s: %s", strings.Join(issues[0].Path, "/"), issues[0].Reason)
	}
	return assets, nil
}

type mediaIssue struct {
	Path   []string
	Reason string
}

func inspectVisualMedia(root any, adapter protocolAdapter) ([]visualAsset, []mediaIssue) {
	obj, _ := root.(map[string]any)
	items := adapter.conversationItems(root)
	base := adapter.conversationField
	var assets []visualAsset
	var issues []mediaIssue
	for itemIndex, item := range items {
		role := ""
		if itemObj, ok := item.(map[string]any); ok {
			role, _ = itemObj["role"].(string)
		}
		walkVisualMedia(item, []string{base, "#" + strconv.Itoa(itemIndex)}, itemIndex, role, adapter, &assets, &issues)
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		if key != base {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		before := len(assets)
		beforeIssues := len(issues)
		walkVisualMedia(obj[key], []string{key}, -1, "", adapter, &assets, &issues)
		for _, asset := range assets[before:] {
			issues = append(issues, mediaIssue{
				Path:   append([]string(nil), asset.Path...),
				Reason: fmt.Sprintf("media outside messages/input is not supported for %s; expected %s", adapter.protocol, base),
			})
		}
		for index := beforeIssues; index < len(issues); index++ {
			if strings.Contains(issues[index].Reason, "incompatible image block") {
				issues[index].Reason += fmt.Sprintf("; expected content under %s", base)
			}
		}
	}
	return assets, issues
}

func walkVisualMedia(value any, path []string, itemIndex int, role string, adapter protocolAdapter, assets *[]visualAsset, issues *[]mediaIssue) {
	switch current := value.(type) {
	case map[string]any:
		typ := strings.ToLower(strings.TrimSpace(stringValue(current["type"])))
		if isImageBlockType(typ) {
			if !adapter.supportsImageType(typ) {
				*issues = append(*issues, mediaIssue{
					Path:   append([]string(nil), path...),
					Reason: fmt.Sprintf("%s request contains incompatible image block type %q; expected %q", adapter.protocol, typ, adapter.imageBlockType),
				})
				return
			}
			rawURL, err := adapter.decodeImageBlock(current, typ)
			if err != nil {
				*issues = append(*issues, mediaIssue{Path: append([]string(nil), path...), Reason: err.Error()})
				return
			}
			id := strings.Join(path, "/")
			*assets = append(*assets, visualAsset{ID: id, URL: rawURL, Path: append([]string(nil), path...), ItemIndex: itemIndex, Role: role})
			return
		}
		if reason := unsupportedMediaReason(current, typ); reason != "" {
			*issues = append(*issues, mediaIssue{Path: append([]string(nil), path...), Reason: reason})
			return
		}
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			walkVisualMedia(current[key], append(path, key), itemIndex, role, adapter, assets, issues)
		}
	case []any:
		for index, child := range current {
			walkVisualMedia(child, append(path, "#"+strconv.Itoa(index)), itemIndex, role, adapter, assets, issues)
		}
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func isImageBlockType(typ string) bool {
	return typ == "image_url" || typ == "input_image" || typ == "image"
}

func unsupportedMediaReason(item map[string]any, typ string) string {
	mediaType := ""
	if source, ok := item["source"].(map[string]any); ok {
		mediaType = strings.ToLower(strings.TrimSpace(stringValue(source["media_type"])))
	}
	if typ == "document" || typ == "pdf" || mediaType == "application/pdf" {
		return "PDF attachments are not supported by the image bridge"
	}
	switch typ {
	case "input_file", "file", "file_url", "audio", "input_audio", "video", "input_video", "screenshot", "computer_screenshot":
		return fmt.Sprintf("unsupported media block type %q", typ)
	}
	if strings.Contains(typ, "image") {
		return fmt.Sprintf("unsupported media block type %q", typ)
	}
	if strings.HasPrefix(mediaType, "image/") || strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/") {
		return fmt.Sprintf("unsupported media block type %q for %s", typ, mediaType)
	}
	return ""
}

func traversalAdapter(root any, provided []protocolAdapter) protocolAdapter {
	if len(provided) > 0 {
		return provided[0]
	}
	adapter, err := detectProtocolAdapterFromRoot(root, false)
	if err == nil {
		return adapter
	}
	return protocolAdapters[protocolOpenAIChat]
}

func latestUserTurn(root any, provided ...protocolAdapter) (int, string) {
	adapter := traversalAdapter(root, provided)
	items := adapter.conversationItems(root)
	for index := len(items) - 1; index >= 0; index-- {
		item, _ := items[index].(map[string]any)
		if item["role"] == "user" {
			text := strings.TrimSpace(directUserText(item))
			if text == "" && historyItemContainsToolResult(item) {
				continue
			}
			return index, text
		}
	}
	return -1, ""
}

func splitCurrentVisualBatch(root any, adapter protocolAdapter, userIndex int, assets []visualAsset) ([]visualAsset, []visualAsset) {
	latestIndex := userIndex
	for _, asset := range assets {
		if asset.ItemIndex > latestIndex {
			latestIndex = asset.ItemIndex
		}
	}
	batchStart := latestIndex
	if latestIndex > userIndex {
		items := adapter.conversationItems(root)
		if latestIndex >= 0 && latestIndex < len(items) && historyItemContainsToolResult(items[latestIndex]) {
			for batchStart > userIndex+1 && historyItemContainsToolResult(items[batchStart-1]) {
				batchStart--
			}
		}
	}
	current := make([]visualAsset, 0)
	historical := make([]visualAsset, 0)
	for _, asset := range assets {
		isCurrent := asset.ItemIndex == userIndex
		if latestIndex > userIndex {
			isCurrent = asset.ItemIndex >= batchStart && asset.ItemIndex <= latestIndex
		}
		if isCurrent {
			current = append(current, asset)
		} else {
			historical = append(historical, asset)
		}
	}
	return current, historical
}

func conversationItems(root any, provided ...protocolAdapter) []any {
	return traversalAdapter(root, provided).conversationItems(root)
}

func nearbyUserTask(root any, asset visualAsset, provided ...protocolAdapter) string {
	adapter := traversalAdapter(root, provided)
	items := adapter.conversationItems(root)
	start := asset.ItemIndex
	if start >= len(items) {
		start = len(items) - 1
	}
	for index := start; index >= 0; index-- {
		item, _ := items[index].(map[string]any)
		if strings.ToLower(strings.TrimSpace(stringValue(item["role"]))) != "user" {
			continue
		}
		if text := strings.TrimSpace(directUserText(item)); text != "" {
			return text
		}
	}
	return ""
}

// directUserText deliberately ignores nested tool_result content. A Claude
// tool result can carry the screenshot and arbitrary tool output, while the
// actual user task lives in the preceding user turn.
func directUserText(item map[string]any) string {
	value := item["content"]
	switch current := value.(type) {
	case string:
		return current
	case []any:
		parts := make([]string, 0)
		for _, block := range current {
			obj, _ := block.(map[string]any)
			if strings.ToLower(strings.TrimSpace(stringValue(obj["type"]))) == "tool_result" {
				continue
			}
			if text, ok := obj["text"].(string); ok {
				parts = append(parts, text)
			}
			if text, ok := obj["input_text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func imageURL(item map[string]any) string {
	if raw, ok := item["image_url"].(string); ok {
		return raw
	}
	if raw, ok := item["url"].(string); ok {
		return raw
	}
	if obj, ok := item["image_url"].(map[string]any); ok {
		if raw, ok := obj["url"].(string); ok {
			return raw
		}
	}
	return ""
}

func validateAsset(raw string, cfg runtimeConfig) error {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "data:") {
		comma := strings.IndexByte(raw, ',')
		if comma < 0 {
			return fmt.Errorf("invalid image data URL")
		}
		metadata := strings.ToLower(raw[5:comma])
		mediaType := strings.TrimSpace(strings.SplitN(metadata, ";", 2)[0])
		if !strings.HasPrefix(mediaType, "image/") {
			if mediaType == "application/pdf" {
				return fmt.Errorf("PDF attachments are not supported by the image bridge")
			}
			return fmt.Errorf("unsupported image data media type %q", mediaType)
		}
		payload := raw[comma+1:]
		var size int
		if strings.Contains(metadata, ";base64") {
			decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(payload))
			decoded, err := io.Copy(io.Discard, io.LimitReader(decoder, int64(cfg.MaxImageDataBytes)+1))
			if err != nil {
				return fmt.Errorf("invalid base64 image data")
			}
			if decoded > int64(cfg.MaxImageDataBytes) {
				return fmt.Errorf("image exceeds the maximum of %d bytes", cfg.MaxImageDataBytes)
			}
			if decoded > int64(^uint(0)>>1) {
				return fmt.Errorf("image data is too large")
			}
			size = int(decoded)
		} else {
			var err error
			size, err = queryUnescapedSize(payload)
			if err != nil {
				return err
			}
		}
		if size > cfg.MaxImageDataBytes {
			return fmt.Errorf("image contains %d bytes; maximum is %d", size, cfg.MaxImageDataBytes)
		}
		return nil
	}
	if !cfg.AllowRemoteImageURLs {
		return fmt.Errorf("remote image URLs are disabled")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("unsupported image URL")
	}
	if strings.HasSuffix(strings.ToLower(parsed.Path), ".pdf") {
		return fmt.Errorf("PDF attachments are not supported by the image bridge")
	}
	return nil
}

func queryUnescapedSize(payload string) (int, error) {
	size := 0
	for index := 0; index < len(payload); index++ {
		if payload[index] != '%' {
			size++
			continue
		}
		if index+2 >= len(payload) || !isHex(payload[index+1]) || !isHex(payload[index+2]) {
			return 0, fmt.Errorf("invalid image data URL")
		}
		size++
		index += 2
	}
	return size, nil
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func validateArchivedAssetMetadata(raw string, cfg runtimeConfig) error {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		if !cfg.AllowRemoteImageURLs {
			return fmt.Errorf("remote image URLs are disabled")
		}
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("unsupported image URL")
		}
		if strings.HasSuffix(strings.ToLower(parsed.Path), ".pdf") {
			return fmt.Errorf("PDF attachments are not supported by the image bridge")
		}
		return nil
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return fmt.Errorf("invalid image data URL")
	}
	metadata := strings.ToLower(raw[5:comma])
	mediaType := strings.TrimSpace(strings.SplitN(metadata, ";", 2)[0])
	if mediaType == "application/pdf" {
		return fmt.Errorf("PDF attachments are not supported by the image bridge")
	}
	if !strings.HasPrefix(mediaType, "image/") {
		return fmt.Errorf("unsupported image data media type %q", mediaType)
	}
	return nil
}

func replaceAsset(root any, path []string, description string, adapter protocolAdapter) bool {
	if len(path) == 0 {
		return false
	}
	current := root
	for index, step := range path {
		last := index == len(path)-1
		if strings.HasPrefix(step, "#") {
			position, _ := strconv.Atoi(strings.TrimPrefix(step, "#"))
			array, ok := current.([]any)
			if !ok || position < 0 || position >= len(array) {
				return false
			}
			if last {
				array[position] = adapter.makeTextBlock(description)
				return true
			}
			current = array[position]
		} else {
			obj, ok := current.(map[string]any)
			if !ok {
				return false
			}
			if last {
				obj[step] = adapter.makeTextBlock(description)
				return true
			}
			current = obj[step]
		}
	}
	return false
}
