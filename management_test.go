package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestReleaseVersionMatchesMakefile(t *testing.T) {
	raw, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "VERSION ?= "+defaultVersion) {
		t.Fatalf("Makefile version does not match plugin version %s", defaultVersion)
	}
}

func TestResourceRouteReturnsStaticManagementShell(t *testing.T) {
	req, err := json.Marshal(pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/v0/resource/plugins/glm-vision-bridge/open",
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := managementHandle(req)
	if err != nil {
		t.Fatal(err)
	}
	var reply envelope
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if !reply.OK {
		t.Fatalf("management response failed: %s", raw)
	}
	var response managementResponse
	if err := json.Unmarshal(reply.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if string(response.Body) != managementTemplate {
		t.Fatal("resource route did not return the embedded static template verbatim")
	}
	for _, forbidden := range []string{
		"__GLM_VISION_BRIDGE_DATA__",
		`"events":[`,
		`"primary_model":`,
		`"model":"`,
	} {
		if strings.Contains(string(response.Body), forbidden) {
			t.Fatalf("resource route contains dynamic payload marker %q", forbidden)
		}
	}
}

func TestManagementShellReusesStoredCredentialWithoutPasswordField(t *testing.T) {
	html := managementHTML()
	for _, want := range []string{
		"storedManagementKey()",
		"localStorage.getItem('cli-proxy-auth')",
		"localStorage.getItem('managementKey')",
		"enc::v1::",
		"Authorization:'Bearer '+managementKey",
		"window.__loadMgmtData();",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("management shell missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`id="mkey"`,
		`type="password"`,
		"管理密钥",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("management shell still contains password UI %q", forbidden)
		}
	}
}
