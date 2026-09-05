package config

import (
	"testing"
	"time"
)

func TestParseDefaultsAndDurations(t *testing.T) {
	cfg, err := Parse([]byte(`
enabled: true
auto_wake: true
scan_interval: 30s
request_timeout: 45s
state_file: plugins/codex-wakeup/state.json
history_limit: 12
default_model: gpt-test
default_prompt: "only reply OK"
default_max_output_tokens: 7
run_on_start: true
upstream_url: https://chatgpt.com/backend-api/codex/responses
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Enabled || !cfg.AutoWake || !cfg.RunOnStart {
		t.Fatalf("boolean options were not parsed: %#v", cfg)
	}
	if cfg.ScanInterval != 30*time.Second || cfg.RequestTimeout != 45*time.Second {
		t.Fatalf("durations = %s/%s", cfg.ScanInterval, cfg.RequestTimeout)
	}
	if cfg.DefaultMaxTokens != 7 || cfg.HistoryLimit != 12 {
		t.Fatalf("numeric options = %#v", cfg)
	}
	if cfg.UpstreamURL != DefaultUpstreamURL {
		t.Fatalf("upstream URL was not fixed: %q", cfg.UpstreamURL)
	}
}

func TestParseRejectsUnsafeStateFileAndURL(t *testing.T) {
	for _, raw := range []string{
		`state_file: ../state.json`,
		`state_file: /tmp/state.json`,
		`state_file: state.txt`,
		`upstream_url: http://example.test`,
		`upstream_url: https://evil.example/steal`,
		`upstream_url: https://chatgpt.com/backend-api/codex/responses?token=secret`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%q) succeeded", raw)
		}
	}
}

func TestParseTaskInterval(t *testing.T) {
	if got, err := ParseTaskInterval(""); err != nil || got != DefaultTaskInterval {
		t.Fatalf("empty interval = %s, %v", got, err)
	}
	if got, err := ParseTaskInterval("5h"); err != nil || got != 5*time.Hour {
		t.Fatalf("5h interval = %s, %v", got, err)
	}
}
