package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultScanInterval    = 30 * time.Second
	DefaultRequestTimeout  = 60 * time.Second
	DefaultStateFile       = "plugins/codex-wakeup/state.json"
	DefaultHistoryLimit    = 300
	DefaultModel           = "gpt-5.3-codex"
	DefaultPrompt          = "hi"
	DefaultMaxOutputTokens = 32
	DefaultUpstreamURL     = "https://chatgpt.com/backend-api/codex/responses"
	DefaultTaskInterval    = 5 * time.Hour
	minimumScanInterval    = time.Second
	maximumScanInterval    = 24 * time.Hour
	minimumRequestTimeout  = time.Second
	maximumRequestTimeout  = 10 * time.Minute
	maximumHistoryLimit    = 10000
	maximumOutputTokens    = 4096
)

var ErrInvalidConfig = errors.New("invalid codex-wakeup configuration")

// Config is the validated plugin configuration. The host-owned enabled field
// is included because CPA forwards it in config_yaml during registration.
type Config struct {
	Enabled        bool
	AutoWake       bool
	ScanInterval   time.Duration
	StateFile      string
	HistoryLimit   int
	RequestTimeout time.Duration
	DefaultModel   string
	DefaultPrompt  string
	// DefaultMaxTokens is retained for config compatibility. The ChatGPT
	// Codex backend rejects max_output_tokens, so it is not sent outbound.
	DefaultMaxTokens int
	RunOnStart       bool
	UpstreamURL      string
}

type rawConfig struct {
	Enabled          any `yaml:"enabled"`
	AutoWake         any `yaml:"auto_wake"`
	ScanInterval     any `yaml:"scan_interval"`
	StateFile        any `yaml:"state_file"`
	HistoryLimit     any `yaml:"history_limit"`
	RequestTimeout   any `yaml:"request_timeout"`
	DefaultModel     any `yaml:"default_model"`
	DefaultPrompt    any `yaml:"default_prompt"`
	DefaultMaxTokens any `yaml:"default_max_output_tokens"`
	RunOnStart       any `yaml:"run_on_start"`
	UpstreamURL      any `yaml:"upstream_url"`
}

func Default() Config {
	return Config{
		Enabled:          false,
		AutoWake:         false,
		ScanInterval:     DefaultScanInterval,
		StateFile:        DefaultStateFile,
		HistoryLimit:     DefaultHistoryLimit,
		RequestTimeout:   DefaultRequestTimeout,
		DefaultModel:     DefaultModel,
		DefaultPrompt:    DefaultPrompt,
		DefaultMaxTokens: DefaultMaxOutputTokens,
		RunOnStart:       false,
		UpstreamURL:      DefaultUpstreamURL,
	}
}

// Parse accepts the YAML subtree supplied by CPA. JSON is also valid YAML,
// which makes this function convenient for unit tests and management calls.
func Parse(raw []byte) (Config, error) {
	cfg := Default()
	var decoded rawConfig
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &decoded); err != nil {
			return Config{}, fmt.Errorf("%w: decode YAML: %v", ErrInvalidConfig, err)
		}
	}

	var err error
	if decoded.Enabled != nil {
		cfg.Enabled, err = boolValue(decoded.Enabled, "enabled")
		if err != nil {
			return Config{}, err
		}
	}
	if decoded.AutoWake != nil {
		cfg.AutoWake, err = boolValue(decoded.AutoWake, "auto_wake")
		if err != nil {
			return Config{}, err
		}
	}
	if decoded.RunOnStart != nil {
		cfg.RunOnStart, err = boolValue(decoded.RunOnStart, "run_on_start")
		if err != nil {
			return Config{}, err
		}
	}
	if decoded.ScanInterval != nil {
		cfg.ScanInterval, err = parseDuration(decoded.ScanInterval, "scan_interval", minimumScanInterval, maximumScanInterval)
		if err != nil {
			return Config{}, err
		}
	}
	if decoded.RequestTimeout != nil {
		cfg.RequestTimeout, err = parseDuration(decoded.RequestTimeout, "request_timeout", minimumRequestTimeout, maximumRequestTimeout)
		if err != nil {
			return Config{}, err
		}
	}
	if decoded.StateFile != nil {
		cfg.StateFile = strings.TrimSpace(stringValue(decoded.StateFile))
	}
	if err = ValidateStateFile(cfg.StateFile); err != nil {
		return Config{}, err
	}
	if decoded.HistoryLimit != nil {
		cfg.HistoryLimit, err = intValue(decoded.HistoryLimit, "history_limit")
		if err != nil {
			return Config{}, err
		}
		if cfg.HistoryLimit < 1 || cfg.HistoryLimit > maximumHistoryLimit {
			return Config{}, fmt.Errorf("%w: history_limit must be between 1 and %d", ErrInvalidConfig, maximumHistoryLimit)
		}
	}
	if decoded.DefaultModel != nil {
		cfg.DefaultModel = strings.TrimSpace(stringValue(decoded.DefaultModel))
	}
	if cfg.DefaultModel == "" {
		return Config{}, fmt.Errorf("%w: default_model is required", ErrInvalidConfig)
	}
	if decoded.DefaultPrompt != nil {
		cfg.DefaultPrompt = strings.TrimSpace(stringValue(decoded.DefaultPrompt))
	}
	if cfg.DefaultPrompt == "" {
		return Config{}, fmt.Errorf("%w: default_prompt is required", ErrInvalidConfig)
	}
	if decoded.DefaultMaxTokens != nil {
		cfg.DefaultMaxTokens, err = intValue(decoded.DefaultMaxTokens, "default_max_output_tokens")
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.DefaultMaxTokens < 1 || cfg.DefaultMaxTokens > maximumOutputTokens {
		return Config{}, fmt.Errorf("%w: default_max_output_tokens must be between 1 and %d", ErrInvalidConfig, maximumOutputTokens)
	}
	if decoded.UpstreamURL != nil {
		// Keep parsing the historical field so an unsafe old configuration
		// fails closed. The plugin never permits a caller-selected destination.
		if err = ValidateUpstreamURL(strings.TrimSpace(stringValue(decoded.UpstreamURL))); err != nil {
			return Config{}, err
		}
	}
	cfg.UpstreamURL = DefaultUpstreamURL
	return cfg, nil
}

func ValidateStateFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("%w: state_file is required", ErrInvalidConfig)
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "~") {
		return fmt.Errorf("%w: state_file must be relative", ErrInvalidConfig)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: state_file may not escape the plugin directory", ErrInvalidConfig)
	}
	if strings.ToLower(filepath.Ext(clean)) != ".json" {
		return fmt.Errorf("%w: state_file must end with .json", ErrInvalidConfig)
	}
	return nil
}

func ValidateUpstreamURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "chatgpt.com" || parsed.Path != "/backend-api/codex/responses" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return fmt.Errorf("%w: upstream_url is fixed to the official Codex Responses endpoint", ErrInvalidConfig)
	}
	return nil
}

func ParseTaskInterval(raw string) (time.Duration, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return DefaultTaskInterval, nil
	}
	return parseDuration(text, "task.schedule.interval", time.Minute, 30*24*time.Hour)
}

func parseDuration(raw any, name string, minimum, maximum time.Duration) (time.Duration, error) {
	text := strings.TrimSpace(stringValue(raw))
	if text == "" {
		return 0, fmt.Errorf("%w: %s is required", ErrInvalidConfig, name)
	}
	if number, err := strconv.ParseFloat(text, 64); err == nil {
		text = fmt.Sprintf("%gs", number)
	}
	duration, err := time.ParseDuration(text)
	if err != nil || duration < minimum || duration > maximum {
		return 0, fmt.Errorf("%w: %s must be a duration between %s and %s", ErrInvalidConfig, name, minimum, maximum)
	}
	return duration, nil
}

func stringValue(value any) string {
	switch item := value.(type) {
	case string:
		return item
	case []byte:
		return string(item)
	case int:
		return strconv.Itoa(item)
	case int64:
		return strconv.FormatInt(item, 10)
	case uint64:
		return strconv.FormatUint(item, 10)
	case float64:
		return strconv.FormatFloat(item, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(item)
	default:
		return fmt.Sprint(item)
	}
}

func boolValue(value any, name string) (bool, error) {
	text := strings.TrimSpace(stringValue(value))
	parsed, err := strconv.ParseBool(text)
	if err != nil {
		return false, fmt.Errorf("%w: %s must be boolean", ErrInvalidConfig, name)
	}
	return parsed, nil
}

func intValue(value any, name string) (int, error) {
	text := strings.TrimSpace(stringValue(value))
	parsed, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be an integer", ErrInvalidConfig, name)
	}
	return parsed, nil
}
