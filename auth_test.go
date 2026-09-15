package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
)

func TestEligibleAuthRequiresCodexFileCredential(t *testing.T) {
	base := host.AuthFile{AuthIndex: "a", Provider: "codex", Type: "codex", Source: "file"}
	if !eligibleAuth(base) {
		t.Fatal("expected codex file auth to be eligible")
	}
	for name, entry := range map[string]host.AuthFile{
		"openai provider": {AuthIndex: "a", Provider: "openai", Type: "codex", Source: "file"},
		"oauth type":      {AuthIndex: "a", Provider: "codex", Type: "oauth", Source: "file"},
		"api key":         {AuthIndex: "a", Provider: "codex", Type: "codex", Source: "file", Label: "api"},
		"runtime":         {AuthIndex: "a", Provider: "codex", Type: "codex", Source: "memory", RuntimeOnly: true},
		"empty source":    {AuthIndex: "a", Provider: "codex", Type: "codex"},
		"unknown source":  {AuthIndex: "a", Provider: "codex", Type: "codex", Source: "export"},
		"disabled":        {AuthIndex: "a", Provider: "codex", Type: "codex", Source: "file", Disabled: true},
	} {
		if name == "api key" {
			// Eligibility only describes the host entry; API-key material is
			// rejected by parseCodexCredential below.
			continue
		}
		if eligibleAuth(entry) {
			t.Errorf("%s: unexpectedly eligible: %#v", name, entry)
		}
	}
}

func TestQuotaMonitoringDoesNotRequireHostAvailability(t *testing.T) {
	for _, unavailable := range []host.AuthFile{
		{AuthIndex: "a", Provider: "codex", Type: "codex", Source: "file", Unavailable: true},
		{AuthIndex: "a", Provider: "codex", Type: "codex", Source: "file", Status: "unavailable"},
	} {
		if eligibleAuth(unavailable) {
			t.Fatal("normal wake should still honor host unavailability")
		}
		if !monitorableAuth(unavailable) {
			t.Fatal("temporary host unavailability must not stop quota monitoring")
		}
		unavailable.Disabled = true
		if monitorableAuth(unavailable) {
			t.Fatal("monitoring must honor explicit disable")
		}
		unavailable.Disabled = false
		unavailable.Status = "disabled"
		if monitorableAuth(unavailable) {
			t.Fatal("monitoring must honor disabled status")
		}
	}
}

func TestParseCodexCredentialShapesAndJWTAccount(t *testing.T) {
	jwt := jwtForClaims(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-jwt"},
	})
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"top-level", `{"access_token":"access-top","refresh_token":"refresh-top","account_id":"acct-top"}`, "acct-top"},
		{"tokens", `{"tokens":{"access_token":"access-nested","refresh_token":"refresh-nested","account_id":"acct-nested"}}`, "acct-nested"},
		{"oauth", `{"oauth":{"accessToken":"access-oauth","idToken":"id-oauth","accountId":"acct-oauth"}}`, "acct-oauth"},
		{"auth", `{"auth":{"access_token":"access-auth","id_token":"id-auth","account_id":"acct-auth"}}`, "acct-auth"},
		{"session", `{"session":{"access_token":"access-session","refresh_token":"refresh-session"}}`, ""},
		{"credentials", `{"credentials":{"access_token":"access-credentials","id_token":"id-credentials"}}`, ""},
		{"jwt account", `{"access_token":"access-jwt","id_token":"` + jwt + `"}`, "acct-jwt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credential, err := parseCodexCredential([]byte(test.raw))
			if err != nil {
				t.Fatalf("parseCodexCredential() error = %v", err)
			}
			if credential.AccessToken == "" || credential.AccountID != test.want {
				t.Fatalf("credential = %#v, want account %q", credential, test.want)
			}
		})
	}
}

func TestParseCodexCredentialRejectsAPIKeyOnly(t *testing.T) {
	for _, raw := range []string{
		`{"api_key":"sk-secret-api-key"}`,
		`{"access_token":"sk-secret-api-key","refresh_token":"refresh"}`,
		`{"tokens":{"api_key":"sk-secret-api-key"}}`,
	} {
		if _, err := parseCodexCredential([]byte(raw)); err == nil {
			t.Errorf("parseCodexCredential(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestSanitizeTextRedactsBearerSecretsAndEmail(t *testing.T) {
	secret := "a-refresh-token-value"
	input := `Authorization: Bearer ` + secret + ` access_token=` + secret + ` refresh_token="` + secret + `" id_token:` + secret + ` user@example.com`
	output := sanitizeText(input)
	if strings.Contains(output, secret) || strings.Contains(output, "user@example.com") {
		t.Fatalf("sanitizeText leaked secret: %q", output)
	}
	if strings.Contains(output, "Bearer "+secret) {
		t.Fatalf("Bearer token was not redacted: %q", output)
	}
}

func TestSanitizeTextRedactsAbsolutePathsWithoutChangingHTTPS(t *testing.T) {
	input := `/home/user/auth/codex.json C:\Users\x\auth.json https://chatgpt.com/backend-api/codex/responses`
	output := sanitizeText(input)
	for _, path := range []string{`/home/user/auth/codex.json`, `C:\Users\x\auth.json`} {
		if strings.Contains(output, path) {
			t.Fatalf("absolute path leaked: %q", output)
		}
	}
	if !strings.Contains(output, "https://chatgpt.com/backend-api/codex/responses") {
		t.Fatalf("official URL was changed: %q", output)
	}
}

func jwtForClaims(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}
