package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const currentImageToolPolicyMarker = "[GLM Vision Bridge current-image policy]"

const currentImageToolPolicy = currentImageToolPolicyMarker + ` The uploaded or referenced image content needed for this turn has already been transcribed into gateway-generated visual memory. Do not use this tool only to locate, open, read, render, emit, display, or re-inspect those images. The tool remains available when the user explicitly requests an operation beyond understanding the image, such as modifying files or code, changing system state, accessing external resources, or processing the image file itself.`

const viewImageGuidanceMarker = "[GLM Vision Bridge view-image guidance]"

const viewImageGuidance = viewImageGuidanceMarker + ` The image content in this conversation has already been analyzed once and transcribed into visual memory above. Use this tool only when you need a specific detail that was not captured in the initial analysis. When calling, include a clear focus on what visual detail you need. Do not use this tool to re-examine the entire image.`

type processedImageToolPolicyResult struct {
	ConstrainedViewImage bool
	ConstrainedTools int
}

func (r processedImageToolPolicyResult) Changed() bool {
	return r.ConstrainedViewImage || r.ConstrainedTools > 0
}

func applyProcessedImageToolPolicy(raw []byte, adapter protocolAdapter) ([]byte, processedImageToolPolicyResult, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, processedImageToolPolicyResult{}, fmt.Errorf("cannot apply processed-image tool policy to invalid request JSON: %w", err)
	}
	result := processedImageToolPolicyResult{}
	apply := func(parent map[string]any) {
		if constrainViewImageDescription(parent, "tools") {
			result.ConstrainedViewImage = true
		}
		result.ConstrainedTools += constrainCurrentImageToolDescriptions(parent, "tools")
	}
	apply(root)
	if adapter.supportsAdditionalTools {
		if input, ok := root["input"].([]any); ok {
			for _, item := range input {
				obj, _ := item.(map[string]any)
				if strings.EqualFold(strings.TrimSpace(stringValue(obj["type"])), "additional_tools") {
					apply(obj)
				}
			}
		}
	}
	// view_image is retained (constrained) so that agents can request
	// targeted re-analysis of already-processed images. tool_choice pointing
	// at view_image is therefore preserved.
	if !result.Changed() {
		return raw, result, nil
	}
	encoded, err := json.Marshal(root)
	return encoded, result, err
}

func constrainCurrentImageToolDescriptions(parent map[string]any, field string) int {
	tools, ok := parent[field].([]any)
	if !ok {
		return 0
	}
	changed := 0
	for _, value := range tools {
		name := toolDefinitionName(value)
		if name != "shell_command" && name != "js" {
			continue
		}
		tool, _ := value.(map[string]any)
		descriptionTarget := tool
		if function, ok := tool["function"].(map[string]any); ok && strings.TrimSpace(stringValue(function["name"])) != "" {
			descriptionTarget = function
		}
		description := strings.TrimSpace(stringValue(descriptionTarget["description"]))
		if strings.Contains(description, currentImageToolPolicyMarker) {
			continue
		}
		if description == "" {
			descriptionTarget["description"] = currentImageToolPolicy
		} else {
			descriptionTarget["description"] = description + "\n\n" + currentImageToolPolicy
		}
		changed++
	}
	return changed
}

func constrainViewImageDescription(parent map[string]any, field string) bool {
	tools, ok := parent[field].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, value := range tools {
		if toolDefinitionName(value) != "view_image" {
			continue
		}
		tool, _ := value.(map[string]any)
		descriptionTarget := tool
		if function, ok := tool["function"].(map[string]any); ok && strings.TrimSpace(stringValue(function["name"])) != "" {
			descriptionTarget = function
		}
		description := strings.TrimSpace(stringValue(descriptionTarget["description"]))
		if strings.Contains(description, viewImageGuidanceMarker) {
			continue
		}
		if description == "" {
			descriptionTarget["description"] = viewImageGuidance
		} else {
			descriptionTarget["description"] = description + "\n\n" + viewImageGuidance
		}
		changed = true
	}
	return changed
}

func filterNamedToolList(parent map[string]any, field, blockedName string) bool {
	tools, ok := parent[field].([]any)
	if !ok {
		return false
	}
	filtered := make([]any, 0, len(tools))
	removed := false
	for _, tool := range tools {
		if toolDefinitionName(tool) == blockedName {
			removed = true
			continue
		}
		filtered = append(filtered, tool)
	}
	if removed {
		parent[field] = filtered
	}
	return removed
}

func toolDefinitionName(value any) string {
	tool, _ := value.(map[string]any)
	if name := strings.TrimSpace(stringValue(tool["name"])); name != "" {
		return name
	}
	function, _ := tool["function"].(map[string]any)
	return strings.TrimSpace(stringValue(function["name"]))
}

func cleanToolChoice(root map[string]any, blockedName string) bool {
	choice, ok := root["tool_choice"]
	if !ok {
		return false
	}
	if name, ok := choice.(string); ok {
		if strings.TrimSpace(name) == blockedName {
			delete(root, "tool_choice")
			return true
		}
		return false
	}
	choiceObject, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	if toolDefinitionName(choiceObject) == blockedName {
		delete(root, "tool_choice")
		return true
	}
	if filterNamedToolList(choiceObject, "tools", blockedName) {
		if tools, _ := choiceObject["tools"].([]any); len(tools) == 0 {
			delete(root, "tool_choice")
		}
		return true
	}
	return false
}
