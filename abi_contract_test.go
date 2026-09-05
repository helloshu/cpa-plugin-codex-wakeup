package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRegistrationWireMatchesCLIProxyAPIMetadata(t *testing.T) {
	raw, err := json.Marshal(pluginRegistration())
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      struct {
			Name             string
			Version          string
			Author           string
			GitHubRepository string
			Logo             string
			ConfigFields     []struct {
				Name        string
				Type        string
				EnumValues  []string
				Description string
			}
		}
		Capabilities struct {
			ManagementAPI bool `json:"management_api"`
		}
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != schemaVersion || decoded.Metadata.Name == "" || decoded.Metadata.Version == "" || decoded.Metadata.Author == "" || decoded.Metadata.GitHubRepository == "" {
		t.Fatalf("registration metadata = %#v", decoded)
	}
	if !decoded.Capabilities.ManagementAPI || len(decoded.Metadata.ConfigFields) == 0 {
		t.Fatalf("registration capabilities/config fields = %#v", decoded)
	}
	for _, field := range decoded.Metadata.ConfigFields {
		if field.Name == "" || field.Type == "" || field.Description == "" {
			t.Fatalf("incomplete config field = %#v", field)
		}
	}
}

func TestRegistrationSchemaNegotiation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want uint32
	}{
		{name: "host schema 4", raw: `{"schema_version":4}`, want: 4},
		{name: "host schema 5", raw: `{"schema_version":5}`, want: 5},
		{name: "future host schema", raw: `{"schema_version":99}`, want: 5},
		{name: "missing schema", raw: `{}`, want: 1},
		{name: "zero schema", raw: `{"schema_version":0}`, want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := pluginRegistrationForSchema(requestedSchemaVersion([]byte(test.raw))).SchemaVersion
			if got != test.want {
				t.Fatalf("negotiated schema = %d, want %d", got, test.want)
			}
		})
	}
}

func TestLifecycleRegistrationUsesNegotiatedSchema(t *testing.T) {
	p := newPluginRuntime()
	t.Cleanup(p.shutdown)
	for _, method := range []string{methodPluginRegister, methodPluginReconfigure} {
		raw, err := p.handleMethod(method, []byte(`{"schema_version":4}`))
		if err != nil {
			t.Fatalf("%s error = %v", method, err)
		}
		var envelope rpcEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("%s envelope: %v", method, err)
		}
		var response registration
		if err := json.Unmarshal(envelope.Result, &response); err != nil {
			t.Fatalf("%s result: %v", method, err)
		}
		if response.SchemaVersion != 4 {
			t.Fatalf("%s schema = %d, want 4", method, response.SchemaVersion)
		}
	}
}

func TestConfigYAMLCurrentAndHistoricalWireShapes(t *testing.T) {
	yaml := []byte("enabled: true\nauto_wake: true\ndefault_prompt: hi\n")
	current, err := json.Marshal(struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}{ConfigYAML: yaml, SchemaVersion: schemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseRegistrationConfig(current)
	if err != nil || !cfg.Enabled || !cfg.AutoWake || cfg.DefaultPrompt != "hi" {
		t.Fatalf("current config = %#v, %v", cfg, err)
	}
	historical, _ := json.Marshal(map[string]any{"config_yaml": string(yaml)})
	cfg, err = parseRegistrationConfig(historical)
	if err != nil || !cfg.Enabled || !cfg.AutoWake {
		t.Fatalf("historical config = %#v, %v", cfg, err)
	}
	if got := base64.StdEncoding.EncodeToString(yaml); !strings.Contains(string(current), got) {
		t.Fatalf("current wire was not base64: %s", current)
	}
}

func TestManagementRegistrationAndResponseWire(t *testing.T) {
	raw, err := json.Marshal(managementRegistration())
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Routes []struct {
			Method string
			Path   string
		}
		Resources []struct {
			Path string
		}
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Routes) == 0 || len(decoded.Resources) != 1 {
		t.Fatalf("management registration = %#v", decoded)
	}
	for _, route := range decoded.Routes {
		if !strings.HasPrefix(route.Path, managementNamespace+"/") {
			t.Fatalf("route is not namespaced: %#v", route)
		}
	}
	if decoded.Resources[0].Path != "/status" {
		t.Fatalf("resource path = %q", decoded.Resources[0].Path)
	}

	response := managementResponse{StatusCode: http.StatusTeapot, Headers: map[string][]string{"Content-Type": {"text/html"}}, Body: []byte("<html>ok</html>")}
	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var hostResponse struct {
		StatusCode int
		Headers    map[string][]string
		Body       []byte
	}
	if err := json.Unmarshal(wire, &hostResponse); err != nil {
		t.Fatal(err)
	}
	if hostResponse.StatusCode != http.StatusTeapot || string(hostResponse.Body) != "<html>ok</html>" {
		t.Fatalf("management response wire = %#v", hostResponse)
	}
	p := newPluginRuntime()
	htmlResponse, err := p.handleManagement(managementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/codex-wakeup/status"})
	if err != nil || htmlResponse.StatusCode != http.StatusOK || !strings.Contains(string(htmlResponse.Body), "Codex 唤醒") {
		t.Fatalf("status resource = %#v, %v", htmlResponse, err)
	}
	csp := strings.Join(htmlResponse.Headers["Content-Security-Policy"], ";")
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("status CSP does not allow same-origin fetch: %q", csp)
	}
}
