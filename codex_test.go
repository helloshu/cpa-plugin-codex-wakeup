package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
)

type captureHost struct {
	response host.HTTPResponse
	request  host.HTTPRequest
	logs     []string
}

type lateResponseHost struct {
	captureHost
	delay time.Duration
}

func (h *lateResponseHost) HTTPDo(_ context.Context, request host.HTTPRequest) (host.HTTPResponse, error) {
	time.Sleep(h.delay)
	h.request = request
	return h.response, nil
}

func (h *captureHost) ListAuthFiles(context.Context) ([]host.AuthFile, error) { return nil, nil }
func (h *captureHost) GetAuthFile(context.Context, string) (host.AuthFile, error) {
	return host.AuthFile{}, nil
}
func (h *captureHost) HTTPDo(_ context.Context, request host.HTTPRequest) (host.HTTPResponse, error) {
	h.request = request
	return h.response, nil
}
func (h *captureHost) Log(_ context.Context, _ string, message string, _ map[string]any) {
	h.logs = append(h.logs, message)
}

func TestExecuteCodexRequestShapeAndUsage(t *testing.T) {
	secret := "oauth-secret-token-123"
	responseBody := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":3,\"total_tokens\":14}}}\n\n"
	fake := &captureHost{response: host.HTTPResponse{StatusCode: 200, Body: []byte(responseBody)}}
	result, err := executeCodex(context.Background(), fake, host.AuthFile{}, codexCredential{AccessToken: secret, AccountID: "acct-1"}, wakeSettings{
		Model:           "gpt-test",
		Prompt:          "hi",
		MaxOutputTokens: 32,
		UpstreamURL:     "https://chatgpt.com/backend-api/codex/responses",
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("executeCodex() error = %v", err)
	}
	if result.InputTokens != 11 || result.OutputTokens != 3 || result.TotalTokens != 14 {
		t.Fatalf("usage = %#v", result)
	}
	var body map[string]any
	if err := json.Unmarshal(fake.request.Body, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "input", "instructions", "reasoning", "include", "parallel_tool_calls", "stream", "store"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("request is missing %q: %#v", key, body)
		}
	}
	if body["stream"] != true || body["store"] != false {
		t.Fatalf("request flags = %#v", body)
	}
	if _, ok := body["max_output_tokens"]; ok {
		t.Fatalf("request must not send max_output_tokens: %#v", body)
	}
	if body["instructions"] != "Reply with exactly OK." {
		t.Fatalf("instructions = %#v", body["instructions"])
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", body["reasoning"])
	}
	include, ok := body["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", body["include"])
	}
	if body["parallel_tool_calls"] != true {
		t.Fatalf("parallel_tool_calls = %#v", body["parallel_tool_calls"])
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) != 1 || input[0].(map[string]any)["type"] != "message" {
		t.Fatalf("input message = %#v", body["input"])
	}
	if got := fake.request.Headers["Originator"][0]; got != "codex-tui" {
		t.Fatalf("Originator = %q", got)
	}
	if got := fake.request.Headers["User-Agent"][0]; got != "codex-tui/0.146.0 (Linux; x86_64)" {
		t.Fatalf("User-Agent = %q", got)
	}
	if _, ok := fake.request.Headers["OpenAI-Beta"]; ok {
		t.Fatal("OpenAI-Beta should not be sent by the official wakeup shape")
	}
	for _, key := range []string{"version", "x-codex-turn-state", "x-codex-turn-metadata", "x-client-request-id", "x-responsesapi-include-timing-metrics", "session-id", "thread-id", "x-codex-window-id"} {
		values, ok := fake.request.Headers[key]
		if !ok || len(values) != 1 || values[0] != "" {
			t.Fatalf("diagnostic header %q = %#v", key, values)
		}
	}
	if got := fake.request.Headers["ChatGPT-Account-Id"][0]; got != "acct-1" {
		t.Fatalf("account header = %q", got)
	}
	if !strings.Contains(fake.request.Headers["Authorization"][0], secret) {
		t.Fatal("fake request did not receive bearer credential")
	}
}

func TestParseCodexSSEAndJSONSuccessAndErrors(t *testing.T) {
	success := []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
	parsed, err := parseCodexResponse(success)
	if err != nil || parsed.InputTokens != 2 || parsed.OutputTokens != 1 {
		t.Fatalf("SSE parse = %#v, %v", parsed, err)
	}
	if _, err := parseCodexResponse([]byte("event: response.failed\ndata: {\"error\":{\"message\":\"bad\"}}\n\n")); err == nil {
		t.Fatal("failed SSE unexpectedly succeeded")
	}
	if _, err := parseCodexResponse([]byte("data: {\"type\":\"error\",\"message\":\"bad\"}\n\n")); err == nil {
		t.Fatal("error SSE unexpectedly succeeded")
	}
	if _, err := parseCodexResponse([]byte("data: {\"type\":\"response.created\"}\n\n")); err == nil {
		t.Fatal("incomplete SSE unexpectedly succeeded")
	}
	jsonSuccess := []byte(`{"id":"resp_123","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`)
	parsed, err = parseCodexResponse(jsonSuccess)
	if err != nil || parsed.TotalTokens != 6 {
		t.Fatalf("JSON parse = %#v, %v", parsed, err)
	}
	if _, err := parseCodexResponse([]byte(`{"error":{"message":"usage limit Bearer secret-token"}}`)); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("JSON error = %v", err)
	}
	for _, status := range []string{"queued", "in_progress", "running", "pending"} {
		body := []byte(`{"id":"resp_queued","status":"` + status + `"}`)
		if _, err := parseCodexResponse(body); err == nil {
			t.Errorf("non-terminal status %q unexpectedly succeeded", status)
		}
	}
}

func TestExecuteCodexRejectsHTTP2xxWithoutStructure(t *testing.T) {
	fake := &captureHost{response: host.HTTPResponse{StatusCode: 200, Body: []byte(`{"ok":true}`)}}
	_, err := executeCodex(context.Background(), fake, host.AuthFile{}, codexCredential{AccessToken: "secret-token"}, wakeSettings{
		Model: "gpt-test", Prompt: "hi", MaxOutputTokens: 1, UpstreamURL: "https://chatgpt.com/backend-api/codex/responses", Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("structurally invalid 2xx response unexpectedly succeeded")
	}
}

func TestExecuteCodexRejectsLateSuccessAfterContextDeadline(t *testing.T) {
	fake := &lateResponseHost{captureHost: captureHost{response: host.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp_late","status":"completed"}`)}}, delay: 25 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := executeCodex(ctx, fake, host.AuthFile{}, codexCredential{AccessToken: "secret-token"}, wakeSettings{
		Model: "gpt-test", Prompt: "hi", MaxOutputTokens: 1, UpstreamURL: "https://chatgpt.com/backend-api/codex/responses", Timeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("late success was accepted: %v", err)
	}
}

func TestSummarizeHTTPErrorDoesNotLeakBodySecrets(t *testing.T) {
	secret := "oauth-secret-token-123"
	message := summarizeHTTPError([]byte(`{"error":{"message":"Bearer ` + secret + `"},"raw":"` + secret + `"}`))
	if strings.Contains(message, secret) || strings.Contains(message, "Authorization") {
		t.Fatalf("HTTP error leaked secret: %q", message)
	}
}
