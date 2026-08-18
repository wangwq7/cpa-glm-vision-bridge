package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func testRuntime() runtimeConfig {
	cfg := defaultPluginConfig()
	normalized, _ := normalizeConfig(cfg)
	return runtimeConfig{pluginConfig: normalized, cache: newMemoCache(8, ""), events: newEventStore(100)}
}

func TestDefaultConfigUsesBenchmarkedProductionProfile(t *testing.T) {
	cfg, err := normalizeConfig(defaultPluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrimaryModel != "glm-5.2" || len(cfg.TextFallbackModels) != 2 || cfg.TextFallbackModels[0] != "gpt-5.5" || cfg.TextFallbackModels[1] != "gpt-5.6-sol" {
		t.Fatalf("unexpected text chain: primary=%q fallbacks=%#v", cfg.PrimaryModel, cfg.TextFallbackModels)
	}
	if cfg.PrimaryOutputTokenLimit != 64000 {
		t.Fatalf("primary output limit=%d, want 64000", cfg.PrimaryOutputTokenLimit)
	}
	wantVision := []string{"gemini-3.1-flash-lite", "gpt-5.6-terra", "grok-4.5", "claude-sonnet-4-6"}
	if len(cfg.VisionModels) != len(wantVision) {
		t.Fatalf("unexpected visual chain: %#v", cfg.VisionModels)
	}
	for index, want := range wantVision {
		if cfg.VisionModels[index].Model != want {
			t.Fatalf("visual model %d = %q, want %q", index, cfg.VisionModels[index].Model, want)
		}
	}
	if cfg.VisionModels[0].TimeoutSeconds != 20 || cfg.VisionCancelGraceSeconds != 15 {
		t.Fatalf("unexpected visual timing: timeout=%d grace=%d", cfg.VisionModels[0].TimeoutSeconds, cfg.VisionCancelGraceSeconds)
	}
}

func TestPrimaryOutputLimitUsesAndValidatesContextHeadroom(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.PrimaryContextTokens = 128000
	cfg.PrimaryContextBudgetTokens = 100000
	cfg.PrimaryOutputTokenLimit = 0
	cfg.AutoCompressionThresholdTokens = 90000

	got, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrimaryOutputTokenLimit != 28000 {
		t.Fatalf("default output limit=%d, want available headroom 28000", got.PrimaryOutputTokenLimit)
	}

	cfg.PrimaryOutputTokenLimit = 30000
	if _, err := normalizeConfig(cfg); err == nil || !strings.Contains(err.Error(), "primary_output_token_limit") {
		t.Fatalf("expected output headroom validation error, got %v", err)
	}
}

func TestDefaultVisionPromptRequiresVerbatimOCR(t *testing.T) {
	for _, instruction := range []string{"starts directly", "Do not add a SUMMARY section by default", "never repeat", "timestamps", "table cells verbatim", "Do not normalize", "keep values from separate UI regions separate", "instead of guessing", "concise elsewhere"} {
		if !strings.Contains(defaultVisionPrompt, instruction) {
			t.Fatalf("default vision prompt is missing %q", instruction)
		}
	}
}

func TestHistoricalImageReferenceDetectionIsExplicit(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{text: "请重新查看上图", want: 1},
		{text: "分析这张截图", want: 1},
		{text: "compare the previous image with this result", want: 1},
		{text: "比较这两张图片", want: 3},
		{text: "review all screenshots", want: 3},
		{text: "图片多了以后为什么会变慢", want: 0},
		{text: "继续处理这个文件", want: 0},
		{text: "document the image cache behavior", want: 0},
		{text: "全部图片都读取成功了。开始吧", want: 0},
		{text: "所有截图 already analyzed，继续开工", want: 0},
		{text: "请重新识别这张截图", want: 1},
		{text: "继续分析代码", want: 0},
	}
	for _, test := range tests {
		if got := historicalImageRestoreCount(test.text, 3); got != test.want {
			t.Fatalf("text=%q restore=%d, want %d", test.text, got, test.want)
		}
	}
}

func TestVisionRequestUsesHighImageDetail(t *testing.T) {
	raw := makeVisionRequest("vision-a", defaultVisionPrompt, "读取截图", "data:image/png;base64,YQ==")
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	messages := request["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	image := content[1].(map[string]any)["image_url"].(map[string]any)
	if image["detail"] != "high" {
		t.Fatalf("image detail = %#v", image["detail"])
	}
}

func TestTransformOpenAIRequestReplacesImageAndPreservesText(t *testing.T) {
	raw := []byte(`{"model":"glm-vision-bridge","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`)
	got, count, err := transformOpenAIRequest(raw, testRuntime(), func(a visualAsset, context string) (string, error) {
		if a.URL != "https://example.test/a.png" {
			t.Fatal(a.URL)
		}
		if !strings.Contains(context, "what is this?") {
			t.Fatal(context)
		}
		return "A blue square.", nil
	})
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if strings.Contains(string(got), "https://example.test/a.png") || !strings.Contains(string(got), "A blue square.") {
		t.Fatalf("unexpected: %s", got)
	}
}

func TestTransformRespectsRemoteURLPolicy(t *testing.T) {
	r := testRuntime()
	r.AllowRemoteImageURLs = false
	_, _, err := transformOpenAIRequest([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`), r, func(asset visualAsset, _ string) (string, error) { return "", validateAsset(asset.URL, r) })
	if err == nil {
		t.Fatal("expected URL policy error")
	}
}

func TestOversizedBase64ValidationStopsAtConfiguredLimit(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	r.MaxImageDataBytes = 3
	err := validateAsset("data:image/png;base64,"+strings.Repeat("YQ==", 100000), r)
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("err=%v", err)
	}
}

func TestVisionRequestAndResponse(t *testing.T) {
	request := makeVisionRequest("vision", "prompt", "context", "data:image/png;base64,a")
	var decoded map[string]any
	if err := json.Unmarshal(request, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["model"] != "vision(low)" || decoded["reasoning_effort"] != "low" || decoded["stream"] != true {
		t.Fatal(decoded)
	}
	if _, exists := decoded["max_tokens"]; exists {
		t.Fatalf("visual request retained top-level max_tokens: %s", request)
	}
	if got := extractVisionText([]byte(`{"choices":[{"message":{"content":"diagram: one box"}}]}`)); got != "diagram: one box" {
		t.Fatal(got)
	}
}

func TestVisionCancelGraceDefaultsAndCaps(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.VisionCancelGraceSeconds = 0
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.VisionCancelGraceSeconds != 15 {
		t.Fatalf("default grace = %d, want 15", normalized.VisionCancelGraceSeconds)
	}
	cfg.VisionCancelGraceSeconds = 999
	normalized, err = normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.VisionCancelGraceSeconds != 120 {
		t.Fatalf("capped grace = %d, want 120", normalized.VisionCancelGraceSeconds)
	}
}

func TestVisionModelConfigurationPreservesIndependentBudgets(t *testing.T) {
	raw := []byte(`
enabled: true
public_model: bridge
primary_model: text
vision_models:
  - model: vision
    context_limit: 128000
    context_budget: 96000
    timeout_seconds: 37
`)
	var cfg pluginConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.VisionModels) != 1 {
		t.Fatalf("vision models=%#v", normalized.VisionModels)
	}
	model := normalized.VisionModels[0]
	if model.Model != "vision" || model.ContextLimit != 128000 || model.ContextBudget != 96000 || model.TimeoutSeconds != 37 {
		t.Fatalf("vision model=%#v", model)
	}
}

func TestTooManyNewImagesAreRejectedBeforeAnyVisionCall(t *testing.T) {
	r := testRuntime()
	r.MaxImagesPerRequest = 1
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}},{"type":"image_url","image_url":{"url":"https://example.test/b.png"}}]}]}`)
	calls := 0
	_, count, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "should not be called", nil
	})
	if err == nil || calls != 0 || count != 2 {
		t.Fatalf("count=%d calls=%d err=%v, want preflight rejection", count, calls, err)
	}
}

func TestCachedHistoricalImageIsCompactedWithoutVisionCall(t *testing.T) {
	r := testRuntime()
	r.MaxImagesPerRequest = 1
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},{"role":"assistant","content":"看到了"},{"role":"user","content":"继续讨论代码"}]}`)
	var root any
	_ = json.Unmarshal(raw, &root)
	asset := collectVisualAssets(root)[0]
	context := trimToTokens(nearbyUserTask(root, asset), r.VisionInputTokenBudget)
	r.cache.set(visualCacheKey(r, asset, context), "vision", "cached visual memory with details", time.Hour)
	calls := 0
	got, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) { calls++; return "", nil })
	if err != nil || calls != 0 || !strings.Contains(string(got), "历史图片附件已归档") || strings.Contains(string(got), "data:image") {
		t.Fatalf("calls=%d err=%v body=%s", calls, err, got)
	}
}

func TestGenericFileFollowUpDoesNotRestoreHistoricalImage(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},{"role":"assistant","content":"看到了"},{"role":"user","content":"继续处理这个文件"}]}`)
	calls := 0
	got, count, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err != nil || count != 1 || calls != 0 || !strings.Contains(string(got), "历史图片附件已归档") {
		t.Fatalf("count=%d calls=%d err=%v body=%s", count, calls, err, got)
	}
}

func TestSingularAndPluralHistoryReferencesRestoreExpectedImages(t *testing.T) {
	rawTemplate := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},{"role":"assistant","content":"第一张"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Yg=="}}]},{"role":"assistant","content":"第二张"},{"role":"user","content":%q}]}`
	for _, test := range []struct {
		text string
		want int
	}{
		{text: "请重新查看上图", want: 1},
		{text: "请比较这两张图片", want: 2},
	} {
		r := testRuntime()
		r.HistoryRestoreMaxAttachments = 2
		var calls atomic.Int32
		raw := []byte(fmt.Sprintf(rawTemplate, test.text))
		_, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
			calls.Add(1)
			return "restored", nil
		})
		r.cache.close()
		if err != nil || calls.Load() != int32(test.want) {
			t.Fatalf("text=%q calls=%d err=%v, want %d", test.text, calls.Load(), err, test.want)
		}
	}
}

func TestManyUnreferencedHistoricalImagesSkipDecodeAndStayCompact(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	items := make([]any, 0, 81)
	for index := 0; index < 40; index++ {
		image := map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": fmt.Sprintf("data:image/png;base64,not-valid-%d", index)},
			}},
		}
		items = append(items,
			image,
			map[string]any{"role": "assistant", "content": "seen"},
		)
	}
	items = append(items, map[string]any{"role": "user", "content": "continue discussing the code"})
	raw, _ := json.Marshal(map[string]any{"messages": items})
	calls := 0
	got, count, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err != nil || count != 40 || calls != 0 {
		t.Fatalf("count=%d calls=%d err=%v", count, calls, err)
	}
	text := string(got)
	if strings.Count(text, "历史图片附件已归档") != 1 || strings.Contains(text, "not-valid-") || len(got) > 8000 {
		t.Fatalf("historical image output was not bounded: bytes=%d body=%s", len(got), got)
	}
}

func TestReferencedHistoricalImageIsStillStrictlyValidated(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,not-valid"}}]},{"role":"assistant","content":"seen"},{"role":"user","content":"请重新查看上图"}]}`)
	calls := 0
	_, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid base64") || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestHistoricalImageBlockWithPDFMediaTypeStillFailsClosed(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:application/pdf;base64,JVBERi0="}}]},{"role":"assistant","content":"seen"},{"role":"user","content":"continue the code discussion"}]}`)
	calls := 0
	_, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "PDF") || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestHistoricalRemotePDFImageURLStillFailsClosed(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/archive.pdf"}}]},{"role":"assistant","content":"seen"},{"role":"user","content":"continue the code discussion"}]}`)
	_, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "PDF") {
		t.Fatalf("err=%v", err)
	}
}

func TestAllCachedHistoryImagesAreRewritten(t *testing.T) {
	r := testRuntime()
	r.MaxImagesPerRequest = 2
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/one.png"}},{"type":"image_url","image_url":{"url":"https://example.test/two.png"}}]}]}`)
	got, count, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) { return "cached visual memory", nil })
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if strings.Contains(string(got), `"image_url"`) || strings.Contains(string(got), "one.png") || strings.Contains(string(got), "two.png") {
		t.Fatalf("raw images were not fully replaced: %s", got)
	}
}

func TestVisionModelsKeepOrderAndIndependentSettings(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.VisionModels = []visionModel{
		{Model: "vision-a", ContextLimit: 256000, ContextBudget: 120000, TimeoutSeconds: 17},
		{Model: "vision-b", ContextLimit: 128000, ContextBudget: 64000, TimeoutSeconds: 29},
	}
	got, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.VisionModels) != 2 || got.VisionModels[0].Model != "vision-a" || got.VisionModels[1].Model != "vision-b" {
		t.Fatalf("unexpected chain: %#v", got.VisionModels)
	}
	if got.VisionModels[0].ContextLimit != 256000 || got.VisionModels[1].TimeoutSeconds != 29 {
		t.Fatalf("independent settings were not preserved: %#v", got.VisionModels)
	}
}

func TestVisionChainCannotPointBackToBridge(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.VisionModels[0].Model = cfg.PublicModel
	_, err := normalizeConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot point back to the bridge") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeRejectsTextVisionModelOverlap(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.PrimaryModel = "glm-5.2"
	cfg.TextFallbackModels = []string{"gpt-5.5", "gpt-5.6-terra"}
	cfg.VisionModels = []visionModel{{Model: "gpt-5.4-mini"}, {Model: "gpt-5.6-terra"}}
	if _, err := normalizeConfig(cfg); err == nil || !strings.Contains(err.Error(), "both text and visual") {
		t.Fatalf("expected text/vision overlap error, got %v", err)
	}
}

func TestEventStoreKeepsBoundedSanitizedHistory(t *testing.T) {
	store := newEventStore(1)
	first := store.begin("bridge", "glm", false)
	store.stage(first, "视觉识别完成", "完成", "vision", strings.Repeat("x", 700), time.Now())
	store.finish(first, nil)
	_ = store.begin("bridge", "glm", true)
	events := store.snapshot()
	if len(events) != 1 || !events[0].Stream {
		t.Fatalf("unexpected events: %#v", events)
	}
	if got := abbreviateEventText(strings.Repeat("x", 700), 20); !strings.HasSuffix(got, "…") {
		t.Fatalf("expected abbreviated text: %q", got)
	}
}

func TestCPALocalAPISettings(t *testing.T) {
	port, key := cpaLocalAPISettings([]byte("port: 9123\napi-keys:\n  - test-key\n"))
	if port != 9123 || key != "test-key" {
		t.Fatalf("settings = (%d, %q), want (9123, test-key)", port, key)
	}
	port, key = cpaLocalAPISettings([]byte("api-keys: []\n"))
	if port != defaultCPAManagementPort || key != "" {
		t.Fatalf("empty settings = (%d, %q)", port, key)
	}
}

func TestParallelToolResultImagesStayInCurrentTaskThenUseStableMemory(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	first := []byte(`{"model":"glm-vision-bridge","messages":[
		{"role":"user","content":"读取这些图片"},
		{"role":"assistant","content":"reading"},
		{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},
		{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Yg=="}}]},
		{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Yw=="}}]}
	]}`)
	var calls atomic.Int32
	got, count, err := transformOpenAIRequest(first, r, func(visualAsset, string) (string, error) {
		calls.Add(1)
		return "visual memory", nil
	})
	if err != nil || count != 3 || calls.Load() != 3 {
		t.Fatalf("first count=%d calls=%d err=%v", count, calls.Load(), err)
	}
	if strings.Contains(string(got), "data:image") || strings.Contains(string(got), "[旧图已归档]") {
		t.Fatalf("parallel tool images were not fully processed: %s", got)
	}

	second := []byte(`{"model":"glm-vision-bridge","messages":[
		{"role":"user","content":"读取这些图片"},
		{"role":"assistant","content":"reading"},
		{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},
		{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Yg=="}}]},
		{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Yw=="}}]},
		{"role":"user","content":"全部图片都读取成功了。开始吧"}
	]}`)
	var secondCalls atomic.Int32
	got, count, err = transformOpenAIRequest(second, r, func(visualAsset, string) (string, error) {
		secondCalls.Add(1)
		return "unexpected", nil
	})
	if err != nil || count != 3 || secondCalls.Load() != 0 {
		t.Fatalf("second count=%d calls=%d err=%v", count, secondCalls.Load(), err)
	}
	if strings.Count(string(got), "[历史图片识别结果") != 3 || strings.Contains(string(got), "data:image") {
		t.Fatalf("stable visual memories were not reused: %s", got)
	}
}

func TestParallelResponsesToolOutputsStayInCurrentBatch(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"model":"glm-vision-bridge","input":[
		{"role":"user","content":[{"type":"input_text","text":"读取这两张图片"}]},
		{"type":"function_call","call_id":"call-1","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]},
		{"type":"function_call_output","call_id":"call-2","output":[{"type":"input_image","image_url":"data:image/png;base64,Yg=="}]}
	]}`)
	var calls atomic.Int32
	got, count, err := transformRequest(raw, protocolResponses, r, func(_ visualAsset, context string) (string, error) {
		calls.Add(1)
		if !strings.Contains(context, "读取这两张图片") {
			t.Fatalf("context=%q", context)
		}
		return "responses visual memory", nil
	})
	if err != nil || count != 2 || calls.Load() != 2 {
		t.Fatalf("count=%d calls=%d err=%v", count, calls.Load(), err)
	}
	if strings.Contains(string(got), "data:image") || strings.Contains(string(got), "[旧图已归档]") {
		t.Fatalf("responses tool images were not fully processed: %s", got)
	}
}

func TestSequentialToolImageBatchesOnlyProcessLatestBatch(t *testing.T) {
	r := testRuntime()
	r.cache.close()
	r.cache = newMemoCache(64, "")
	defer r.cache.close()
	r.MaxImagesPerRequest = 8
	r.HistoryRestoreMaxAttachments = 0
	items := []any{
		map[string]any{"role": "user", "content": "读取这些图片"},
		map[string]any{"role": "assistant", "content": "first batch"},
	}
	for index := 0; index < 8; index++ {
		items = append(items, map[string]any{
			"role": "tool",
			"content": []any{map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": fmt.Sprintf("data:image/png,old-%d", index)},
			}},
		})
	}
	first, _ := json.Marshal(map[string]any{"model": "glm-vision-bridge", "messages": items})
	var firstCalls atomic.Int32
	if _, count, err := transformOpenAIRequest(first, r, func(visualAsset, string) (string, error) {
		firstCalls.Add(1)
		return "old batch memory", nil
	}); err != nil || count != 8 || firstCalls.Load() != 8 {
		t.Fatalf("first count=%d calls=%d err=%v", count, firstCalls.Load(), err)
	}
	items = append(items, map[string]any{"role": "assistant", "content": "second batch"})
	for index := 0; index < 3; index++ {
		items = append(items, map[string]any{
			"role": "tool",
			"content": []any{map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": fmt.Sprintf("data:image/png,new-%d", index)},
			}},
		})
	}
	second, _ := json.Marshal(map[string]any{"model": "glm-vision-bridge", "messages": items})
	var secondCalls atomic.Int32
	got, count, err := transformOpenAIRequest(second, r, func(visualAsset, string) (string, error) {
		secondCalls.Add(1)
		return "new batch memory", nil
	})
	if err != nil || count != 11 || secondCalls.Load() != 3 {
		t.Fatalf("second count=%d calls=%d err=%v", count, secondCalls.Load(), err)
	}
	if strings.Count(string(got), "[历史图片识别结果") != 8 || strings.Count(string(got), "[图片识别结果") != 3 {
		t.Fatalf("tool batches were not partitioned correctly: %s", got)
	}
}

func TestLatestUserTurnSkipsPureClaudeToolResult(t *testing.T) {
	root := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "读取截图"}}},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-1"}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1"}}},
	}}
	index, text := latestUserTurn(root, protocolAdapters[protocolAnthropic])
	if index != 0 || text != "读取截图" {
		t.Fatalf("latest user turn = (%d, %q)", index, text)
	}
}

func TestExplicitHistoricalReanalysisBypassesStableMemory(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	imageURL := "data:image/png;base64,YQ=="
	asset := visualAsset{URL: imageURL}
	r.cache.set(stableVisualCacheKey(r, asset), "vision", "old OCR", time.Hour)
	raw := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"读取图片"},{"type":"image_url","image_url":{"url":"` + imageURL + `"}}]},
		{"role":"assistant","content":"done"},
		{"role":"user","content":"请重新识别这张截图"}
	]}`)
	calls := 0
	got, _, err := transformOpenAIRequest(raw, r, func(_ visualAsset, context string) (string, error) {
		calls++
		if !strings.Contains(context, "重新识别") {
			t.Fatalf("reanalysis context=%q", context)
		}
		return "fresh OCR", nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if !strings.Contains(string(got), "fresh OCR") || strings.Contains(string(got), "old OCR") {
		t.Fatalf("explicit reanalysis did not replace stable memory: %s", got)
	}
}

func TestStableVisualCacheOnlyUsesInlineImageData(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	if key := stableVisualCacheKey(r, visualAsset{URL: "https://example.test/current.png"}); key != "" {
		t.Fatalf("remote image URL received stable cache key: %q", key)
	}
	if key := stableVisualCacheKey(r, visualAsset{URL: "data:image/png;base64,YQ=="}); key == "" {
		t.Fatal("inline image data did not receive stable cache key")
	}
}

func TestVisionFailureFallbackIsNotPersistedAsStableMemory(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	r.OnVisionFailure = "text_only"
	imageURL := "data:image/png;base64,YQ=="
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"读取图片"},{"type":"image_url","image_url":{"url":"` + imageURL + `"}}]}]}`)
	if _, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		return "", errors.New("temporary vision failure")
	}); err != nil {
		t.Fatal(err)
	}
	if cached, ok := r.cache.get(stableVisualCacheKey(r, visualAsset{URL: imageURL})); ok {
		t.Fatalf("vision failure fallback entered stable cache: %q", cached)
	}
	calls := 0
	got, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "fresh OCR", nil
	})
	if err != nil || calls != 1 || !strings.Contains(string(got), "fresh OCR") {
		t.Fatalf("calls=%d err=%v body=%s", calls, err, got)
	}
}
