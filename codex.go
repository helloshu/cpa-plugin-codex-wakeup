package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/config"
	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
)

type wakeSettings struct {
	Model  string
	Prompt string
	// MaxOutputTokens is retained for task/config wire compatibility. The
	// ChatGPT Codex backend currently rejects max_output_tokens, so it is
	// intentionally not serialized into the outbound request.
	MaxOutputTokens int
	UpstreamURL     string
	Timeout         time.Duration
}
type codexResult struct {
	HTTPStatus   int
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

func executeCodex(ctx context.Context, bridge host.Client, _ host.AuthFile, credential codexCredential, settings wakeSettings) (codexResult, error) {
	if bridge == nil {
		return codexResult{}, errors.New("host bridge is unavailable")
	}
	if settings.Model == "" || settings.Prompt == "" || settings.UpstreamURL != config.DefaultUpstreamURL {
		return codexResult{}, errors.New("wake settings are incomplete")
	}
	body, err := json.Marshal(map[string]any{
		"model": settings.Model,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": settings.Prompt}},
		}},
		// Keep the wakeup turn deliberately short. This is a prompt-level
		// preference, not a hard output-token limit.
		"instructions": "Reply with exactly OK.",
		"reasoning": map[string]any{
			"effort":  "low",
			"summary": "auto",
		},
		"include":             []string{"reasoning.encrypted_content"},
		"parallel_tool_calls": true,
		"stream":              true,
		"store":               false,
	})
	if err != nil {
		return codexResult{}, fmt.Errorf("build Codex request: %w", err)
	}
	headers := make(host.Header)
	for key, value := range map[string]string{
		"Authorization": "Bearer " + credential.AccessToken,
		"Content-Type":  "application/json",
		"Accept":        "text/event-stream",
		"Originator":    "codex-tui",
		"User-Agent":    "codex-tui/0.146.0 (Linux; x86_64)",
	} {
		headers[key] = []string{value}
	}
	for _, key := range []string{
		"version",
		"x-codex-turn-state",
		"x-codex-turn-metadata",
		"x-client-request-id",
		"x-responsesapi-include-timing-metrics",
		"session-id",
		"thread-id",
		"x-codex-window-id",
	} {
		headers[key] = []string{""}
	}
	if credential.AccountID != "" {
		headers["ChatGPT-Account-Id"] = []string{credential.AccountID}
	}
	response, err := bridge.HTTPDo(ctx, host.HTTPRequest{Method: http.MethodPost, URL: settings.UpstreamURL, Headers: headers, Body: body})
	if err != nil {
		return codexResult{}, errors.New(sanitizeWithSecrets("Codex request transport: "+err.Error(), credential.AccessToken))
	}
	if err := contextError(ctx); err != nil {
		return codexResult{}, err
	}
	result := codexResult{HTTPStatus: response.StatusCode}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, fmt.Errorf("Codex request returned HTTP %d: %s", response.StatusCode, sanitizeWithSecrets(summarizeHTTPError(response.Body), credential.AccessToken))
	}
	parsed, err := parseCodexResponse(response.Body)
	if err != nil {
		return result, errors.New(sanitizeWithSecrets(err.Error(), credential.AccessToken))
	}
	result.InputTokens, result.OutputTokens, result.TotalTokens = parsed.InputTokens, parsed.OutputTokens, parsed.TotalTokens
	return result, nil
}

func parseCodexResponse(body []byte) (parsedSSE, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.Contains(trimmed, "event:") || strings.Contains(trimmed, "data:") {
		return parseCodexSSE(body)
	}
	return parseCodexJSON(body)
}

func parseCodexJSON(body []byte) (parsedSSE, error) {
	if len(body) == 0 {
		return parsedSSE{}, errors.New("Codex response was empty")
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		return parsedSSE{}, errors.New("Codex response was not valid JSON")
	}
	if message := jsonErrorMessage(object); message != "" {
		return parsedSSE{}, errors.New(sanitizeText(message))
	}
	var usage parsedSSE
	collectUsage(object, &usage)
	if nested, ok := object["response"].(map[string]any); ok {
		if message := jsonErrorMessage(nested); message != "" {
			return parsedSSE{}, errors.New(sanitizeText(message))
		}
		collectUsage(nested, &usage)
		if jsonHasStructuralSuccess(nested) {
			return usage, nil
		}
	}
	if jsonHasStructuralSuccess(object) {
		return usage, nil
	}
	return parsedSSE{}, errors.New("Codex response lacked a valid completed structure")
}

func jsonErrorMessage(object map[string]any) string {
	if raw, ok := object["error"]; ok {
		switch value := raw.(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return value
			}
		case map[string]any:
			return eventErrorMessage(value)
		default:
			return "Codex response reported an error"
		}
	}
	return ""
}

func jsonHasStructuralSuccess(object map[string]any) bool {
	status := strings.ToLower(strings.TrimSpace(stringValueAt(object, "status")))
	if status != "" && status != "completed" {
		return false
	}
	if id, ok := object["id"].(string); ok && strings.TrimSpace(id) != "" {
		return true
	}
	if output, ok := object["output"].([]any); ok && len(output) > 0 {
		return true
	}
	return status == "completed"
}

type parsedSSE struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

func parseCodexSSE(body []byte) (parsedSSE, error) {
	if len(body) == 0 {
		return parsedSSE{}, errors.New("Codex response was empty")
	}
	if len(body) > 8<<20 {
		return parsedSSE{}, errors.New("Codex response exceeded the safety limit")
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var eventName string
	var dataLines []string
	completed := false
	var usage parsedSSE
	var firstError string
	process := func() {
		if len(dataLines) == 0 {
			eventName = ""
			return
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		name := strings.ToLower(strings.TrimSpace(eventName))
		eventName = ""
		if data == "[DONE]" {
			return
		}
		var object map[string]any
		if json.Unmarshal([]byte(data), &object) != nil {
			if firstError == "" {
				firstError = "malformed SSE data"
			}
			return
		}
		if name == "" {
			if value, ok := object["type"].(string); ok {
				name = strings.ToLower(value)
			}
		}
		if name == "error" || name == "response.failed" || name == "response.incomplete" {
			if firstError == "" {
				firstError = eventErrorMessage(object)
			}
			return
		}
		if name == "response.completed" || name == "response.done" {
			status := strings.ToLower(stringValueAt(object, "status"))
			if nested, ok := object["response"].(map[string]any); ok {
				if status == "" {
					status = strings.ToLower(stringValueAt(nested, "status"))
				}
				collectUsage(nested, &usage)
			}
			collectUsage(object, &usage)
			if status == "" || status == "completed" {
				completed = true
			} else if firstError == "" {
				firstError = "Codex response ended with status " + sanitizeText(status)
			}
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			process()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	process()
	if err := scanner.Err(); err != nil {
		return parsedSSE{}, fmt.Errorf("read Codex SSE: %w", err)
	}
	if firstError != "" {
		return parsedSSE{}, errors.New(sanitizeText(firstError))
	}
	if !completed {
		return parsedSSE{}, errors.New("Codex SSE ended without response.completed")
	}
	return usage, nil
}

func collectUsage(object map[string]any, usage *parsedSSE) {
	if nested, ok := object["usage"].(map[string]any); ok {
		if value := int64Value(nested, "input_tokens", "inputTokens"); value >= 0 {
			usage.InputTokens = value
		}
		if value := int64Value(nested, "output_tokens", "outputTokens"); value >= 0 {
			usage.OutputTokens = value
		}
		if value := int64Value(nested, "total_tokens", "totalTokens"); value >= 0 {
			usage.TotalTokens = value
		}
	}
}
func int64Value(object map[string]any, keys ...string) int64 {
	for key, raw := range object {
		for _, wanted := range keys {
			if canonicalKey(key) != canonicalKey(wanted) {
				continue
			}
			switch value := raw.(type) {
			case float64:
				if value >= 0 {
					return int64(value)
				}
			case json.Number:
				if parsed, err := value.Int64(); err == nil {
					return parsed
				}
			case int64:
				return value
			}
		}
	}
	return -1
}
func stringValueAt(object map[string]any, key string) string {
	for name, value := range object {
		if canonicalKey(name) == canonicalKey(key) {
			if text, ok := value.(string); ok {
				return text
			}
		}
	}
	return ""
}
func eventErrorMessage(object map[string]any) string {
	for _, key := range []string{"message", "error", "detail"} {
		if value, ok := object[key].(string); ok && value != "" {
			return value
		}
		if nested, ok := object[key].(map[string]any); ok {
			if value, ok := nested["message"].(string); ok && value != "" {
				return value
			}
		}
	}
	return "Codex response reported an error"
}
func summarizeHTTPError(body []byte) string {
	var object map[string]any
	if json.Unmarshal(body, &object) == nil {
		if nested, ok := object["error"].(map[string]any); ok {
			if message, ok := nested["message"].(string); ok {
				return sanitizeText(message)
			}
		}
		if message, ok := object["message"].(string); ok {
			return sanitizeText(message)
		}
	}
	return truncateAndScrub(body, 180)
}
