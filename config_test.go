package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExampleConfigMatchesRuntimeContract(t *testing.T) {
	raw, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	node, ok := document.Plugins.Configs[pluginID]
	if !ok {
		t.Fatalf("example config does not contain %q", pluginID)
	}
	pluginYAML, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfigFields(pluginYAML); err != nil {
		t.Fatal(err)
	}
	var cfg pluginConfig
	if err := yaml.Unmarshal(pluginYAML, &cfg); err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.PublicModel != pluginID || len(normalized.VisionModels) != 4 {
		t.Fatalf("unexpected example config: public=%q vision=%#v", normalized.PublicModel, normalized.VisionModels)
	}
}

func TestConfigRejectsUnknownFields(t *testing.T) {
	err := validateConfigFields([]byte("enabled: true\npriority: 100\nunknown_field: value\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown 1.0 configuration field") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeRejectsCompressionRoutingCycles(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "public bridge", model: "glm-vision-bridge", want: "cannot point back"},
		{name: "visual model", model: "gemini-3.1-flash-lite", want: "both text and visual"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := defaultPluginConfig()
			cfg.AutoCompressionModel = test.model
			_, err := normalizeConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestMetadataMatchesRuntimeConfigFields(t *testing.T) {
	want := map[string]bool{}
	configType := reflect.TypeOf(pluginConfig{})
	for index := 0; index < configType.NumField(); index++ {
		name := strings.Split(configType.Field(index).Tag.Get("yaml"), ",")[0]
		if name != "" && name != "-" && name != "enabled" {
			want[name] = true
		}
	}
	got := map[string]bool{}
	for _, field := range metadata().ConfigFields {
		if got[field.Name] {
			t.Fatalf("duplicate metadata field %q", field.Name)
		}
		got[field.Name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata fields do not match runtime config:\ngot  %#v\nwant %#v", got, want)
	}
}

func TestConfigRejectsUnknownVisionModelFields(t *testing.T) {
	err := validateConfigFields([]byte("vision_models:\n  - model: vision\n    max_output_tokens: 1024\n"))
	if err == nil || !strings.Contains(err.Error(), "vision_models[0]") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeRejectsExplicitlyEmptyVisionChain(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.VisionModels = []visionModel{}
	if _, err := normalizeConfig(cfg); err == nil || !strings.Contains(err.Error(), "at least one visual model") {
		t.Fatalf("error = %v", err)
	}
}
