package main

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

func normalizeResponsesStringInput(raw []byte, protocol string) ([]byte, bool, error) {
	body, changed, _, err := normalizeResponsesStringInputWithMedia(raw, protocol)
	return body, changed, err
}

func normalizeResponsesStringInputWithMedia(raw []byte, protocol string) ([]byte, bool, bool, error) {
	if normalizeProtocol(protocol) != "openai-response" {
		return raw, false, false, nil
	}
	scanner := mediaJSONScanner{
		raw:                  raw,
		continueAfterMedia:   true,
		captureTopLevelInput: true,
	}
	scanner.skipSpace()
	if scanner.pos >= len(raw) {
		return nil, false, false, fmt.Errorf("cannot normalize invalid openai-response request JSON")
	}
	rootType := raw[scanner.pos]
	_, valid := scanner.scanValue(0)
	scanner.skipSpace()
	if !valid || scanner.pos != len(raw) {
		return nil, false, false, fmt.Errorf("cannot normalize invalid openai-response request JSON")
	}
	if rootType == 'n' {
		return raw, false, scanner.mediaFound, nil
	}
	if rootType != '{' {
		return nil, false, false, fmt.Errorf("cannot normalize openai-response request: top-level JSON value must be an object")
	}
	if scanner.topLevelInputCount == 0 {
		return raw, false, scanner.mediaFound, nil
	}
	if scanner.topLevelInputCount > 1 {
		return normalizeResponsesStringInputDecodedFallbackWithMedia(raw, scanner.mediaFound)
	}
	input := scanner.topLevelInput
	if input.start >= input.end || raw[input.start] != '"' {
		return raw, false, scanner.mediaFound, nil
	}
	if !utf8.Valid(raw) {
		return normalizeResponsesStringInputDecodedFallbackWithMedia(raw, scanner.mediaFound)
	}
	var text string
	if err := json.Unmarshal(raw[input.start:input.end], &text); err != nil {
		return nil, false, false, fmt.Errorf("cannot decode openai-response string input: %w", err)
	}
	encodedText, err := json.Marshal(text)
	if err != nil {
		return nil, false, false, fmt.Errorf("cannot encode openai-response string input: %w", err)
	}

	const replacementPrefix = `[{"type":"message","role":"user","content":[{"type":"input_text","text":`
	const replacementSuffix = `}]}]`
	body := make([]byte, 0, len(raw)+len(replacementPrefix)+len(replacementSuffix)+len(encodedText)-(input.end-input.start))
	body = append(body, raw[:input.start]...)
	body = append(body, replacementPrefix...)
	body = append(body, encodedText...)
	body = append(body, replacementSuffix...)
	body = append(body, raw[input.end:]...)
	return body, true, scanner.mediaFound, nil
}

func normalizeResponsesStringInputDecodedFallbackWithMedia(raw []byte, unchangedMedia bool) ([]byte, bool, bool, error) {
	body, changed, err := normalizeResponsesStringInputDecodedFallback(raw)
	if err != nil {
		return nil, false, false, err
	}
	if !changed {
		return body, false, unchangedMedia, nil
	}
	media, valid := requestMayContainMedia(body)
	if !valid {
		return nil, false, false, fmt.Errorf("cannot validate normalized openai-response request JSON")
	}
	return body, true, media, nil
}

func normalizeResponsesStringInputDecodedFallback(raw []byte) ([]byte, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false, fmt.Errorf("cannot normalize invalid openai-response request JSON: %w", err)
	}
	inputRaw, exists := root["input"]
	if !exists {
		return raw, false, nil
	}
	var text string
	if err := json.Unmarshal(inputRaw, &text); err != nil {
		return raw, false, nil
	}
	input, err := json.Marshal([]any{map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": text,
		}},
	}})
	if err != nil {
		return nil, false, fmt.Errorf("cannot encode normalized openai-response input: %w", err)
	}
	root["input"] = input
	body, err := json.Marshal(root)
	if err != nil {
		return nil, false, fmt.Errorf("cannot encode normalized openai-response request: %w", err)
	}
	return body, true, nil
}
