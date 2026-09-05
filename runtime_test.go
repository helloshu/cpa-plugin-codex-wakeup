package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/config"
	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/state"
)

type runtimeFakeHost struct {
	mu        sync.Mutex
	entries   []host.AuthFile
	materials map[string][]byte
	responses map[string]host.HTTPResponse
	errors    map[string]error
	requests  []host.HTTPRequest
	logs      []string
	delay     time.Duration
	active    int
	maxActive int
}

type reentrantLogHost struct {
	*runtimeFakeHost
	onLog func()
}

type blockingRuntimeHost struct {
	*runtimeFakeHost
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

func (h *blockingRuntimeHost) HTTPDo(ctx context.Context, request host.HTTPRequest) (host.HTTPResponse, error) {
	h.startedOnce.Do(func() { close(h.started) })
	<-h.release
	return h.runtimeFakeHost.HTTPDo(ctx, request)
}

func (h *reentrantLogHost) Log(ctx context.Context, level, message string, fields map[string]any) {
	h.runtimeFakeHost.Log(ctx, level, message, fields)
	if h.onLog != nil {
		h.onLog()
	}
}

func newRuntimeFakeHost() *runtimeFakeHost {
	entries := []host.AuthFile{
		{ID: "id-a", AuthIndex: "auth-a", Name: "a.json", Label: "A", Email: "alice@example.com", Provider: "codex", Type: "codex", Source: "file"},
		{ID: "id-b", AuthIndex: "auth-b", Name: "b.json", Label: "B", Email: "bob@example.com", Provider: "codex", Type: "codex", Source: "file"},
	}
	return &runtimeFakeHost{
		entries: entries,
		materials: map[string][]byte{
			"auth-a": []byte(`{"access_token":"token-a","account_id":"acct-a"}`),
			"auth-b": []byte(`{"tokens":{"access_token":"token-b","account_id":"acct-b"}}`),
		},
		responses: map[string]host.HTTPResponse{
			"token-a": {StatusCode: 200, Body: []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")},
			"token-b": {StatusCode: 200, Body: []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")},
		},
		errors: make(map[string]error),
	}
}

func (h *runtimeFakeHost) ListAuthFiles(context.Context) ([]host.AuthFile, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]host.AuthFile(nil), h.entries...), nil
}

func (h *runtimeFakeHost) GetAuthFile(_ context.Context, authIndex string) (host.AuthFile, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, ok := h.materials[authIndex]
	if !ok {
		return host.AuthFile{}, fmt.Errorf("missing auth %s", authIndex)
	}
	return host.AuthFile{AuthIndex: authIndex, RawJSON: append([]byte(nil), raw...)}, nil
}

func (h *runtimeFakeHost) HTTPDo(ctx context.Context, request host.HTTPRequest) (host.HTTPResponse, error) {
	token := ""
	if values := request.Headers["Authorization"]; len(values) > 0 {
		token = strings.TrimPrefix(values[0], "Bearer ")
	}
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.active++
	if h.active > h.maxActive {
		h.maxActive = h.active
	}
	delay := h.delay
	response, ok := h.responses[token]
	err := h.errors[token]
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.active--
		h.mu.Unlock()
	}()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return host.HTTPResponse{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	if err != nil {
		return host.HTTPResponse{}, err
	}
	if !ok {
		return host.HTTPResponse{}, errors.New("unexpected fake token")
	}
	return response, nil
}

func (h *runtimeFakeHost) Log(_ context.Context, _ string, message string, _ map[string]any) {
	h.mu.Lock()
	h.logs = append(h.logs, message)
	h.mu.Unlock()
}

func newRuntimeForTest(t *testing.T, fake host.Client) *pluginRuntime {
	t.Helper()
	p := newPluginRuntime()
	p.host = fake
	p.cfg = config.Default()
	p.cfg.Enabled = true
	p.cfg.AutoWake = false
	p.cfg.HistoryLimit = 50
	p.statePath = filepath.Join(t.TempDir(), "state.json")
	p.state = state.Empty()
	p.closed = false
	p.configured = true
	p.startOperations()
	return p
}

func testTask(id string, accounts []string) state.Task {
	now := time.Now().UTC().Add(-6 * time.Hour)
	return state.Task{ID: id, Name: id, Enabled: true, AccountIDs: accounts, CreatedAt: now, NextRunAt: now, Schedule: state.Schedule{Interval: "5h"}}
}

func requestTokens(fake *runtimeFakeHost) []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	result := make([]string, 0, len(fake.requests))
	for _, request := range fake.requests {
		if values := request.Headers["Authorization"]; len(values) > 0 {
			result = append(result, strings.TrimPrefix(values[0], "Bearer "))
		}
	}
	sort.Strings(result)
	return result
}

func TestExecuteTaskSelectionFailureIsolationAndScheduleAdvance(t *testing.T) {
	fake := newRuntimeFakeHost()
	p := newRuntimeForTest(t, fake)
	task := testTask("task-b", []string{"auth-b"})
	p.state.Tasks = []state.Task{task}
	record, err := p.executeTask(context.Background(), task, "schedule", nil, nil, true)
	if err != nil || record.Status != "success" {
		t.Fatalf("task execution = %#v, %v", record, err)
	}
	if got := requestTokens(fake); fmt.Sprint(got) != "[token-b]" {
		t.Fatalf("task account selection = %v", got)
	}
	loaded, err := state.Load(p.statePath)
	if err != nil || len(loaded.History) != 1 {
		t.Fatalf("synchronous state save = %#v, %v", loaded, err)
	}
	if loaded.Tasks[0].LastStatus != "success" || loaded.Tasks[0].NextRunAt.Before(record.CompletedAt.Add(4*time.Hour)) {
		t.Fatalf("schedule was not advanced = %#v", loaded.Tasks[0])
	}

	fake.mu.Lock()
	fake.requests = nil
	fake.responses["token-a"] = host.HTTPResponse{StatusCode: 503, Body: []byte(`{"error":{"message":"temporary"}}`)}
	fake.delay = 5 * time.Millisecond
	fake.mu.Unlock()
	allTask := testTask("task-all", nil)
	p.state.Tasks = append(p.state.Tasks, allTask)
	record, err = p.executeTask(context.Background(), allTask, "schedule", nil, nil, true)
	if err != nil || record.Status != "partial" || len(record.Results) != 2 {
		t.Fatalf("failure isolation = %#v, %v", record, err)
	}
	if got := requestTokens(fake); fmt.Sprint(got) != "[token-a token-b]" {
		t.Fatalf("both accounts were not attempted = %v", got)
	}
	fake.mu.Lock()
	maxActive := fake.maxActive
	fake.mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("accounts in one task were not serial: max_active=%d", maxActive)
	}
	loaded, err = state.Load(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved state.Task
	for _, candidate := range loaded.Tasks {
		if candidate.ID == "task-all" {
			saved = candidate
		}
	}
	if saved.LastStatus != "partial" || saved.NextRunAt.Before(record.CompletedAt.Add(4*time.Hour)) {
		t.Fatalf("failed/partial task did not advance = %#v", saved)
	}
}

func TestWakeSelectionModesAndStableAuthIndex(t *testing.T) {
	fake := newRuntimeFakeHost()
	p := newRuntimeForTest(t, fake)
	task := testTask("task-a", []string{"auth-a"})
	p.state.Tasks = []state.Task{task}

	if _, err := p.wake([]byte(`{"task_id":"task-a"}`)); err != nil {
		t.Fatalf("task wake = %v", err)
	}
	if got := requestTokens(fake); fmt.Sprint(got) != "[token-a]" {
		t.Fatalf("task wake selection = %v", got)
	}
	fake.mu.Lock()
	fake.requests = nil
	fake.mu.Unlock()
	if _, err := p.wake([]byte(`{"auth_indices":["auth-b"]}`)); err != nil {
		t.Fatalf("single account wake = %v", err)
	}
	if got := requestTokens(fake); fmt.Sprint(got) != "[token-b]" {
		t.Fatalf("single account selection = %v", got)
	}
	p.mu.RLock()
	accountB := p.state.Accounts["auth-b"]
	p.mu.RUnlock()
	if accountB.LastStatus != "success" || accountB.SuccessCount != 1 || accountB.LastRunAt.IsZero() {
		t.Fatalf("manual account state was not updated: %#v", accountB)
	}
	fake.mu.Lock()
	fake.requests = nil
	fake.mu.Unlock()
	if _, err := p.wake([]byte(`{}`)); err != nil {
		t.Fatalf("all account wake = %v", err)
	}
	if got := requestTokens(fake); fmt.Sprint(got) != "[token-a token-b]" {
		t.Fatalf("all account selection = %v", got)
	}
	if got := selectCandidates(fake.entries, []string{"A", "alice@example.com", "id-a"}); len(got) != 0 {
		t.Fatalf("unstable selector unexpectedly matched: %#v", got)
	}
	if got := selectCandidates(fake.entries, []string{"auth-a", "auth-a", "id-a"}); len(got) != 1 || got[0].entry.AuthIndex != "auth-a" {
		t.Fatalf("stable selector/dedupe = %#v", got)
	}
}

func TestDelayedStartupTaskRechecksCurrentTask(t *testing.T) {
	fake := newRuntimeFakeHost()
	p := newRuntimeForTest(t, fake)
	created := time.Now().UTC().Add(-time.Minute).Round(0)
	stale := state.Task{ID: "startup", Name: "old", Enabled: true, AccountIDs: []string{"auth-a"}, CreatedAt: created, Schedule: state.Schedule{Kind: state.ScheduleKindStartup}}
	current := stale
	current.Name = "edited"
	current.AccountIDs = []string{"auth-b"}
	p.state.Tasks = []state.Task{current}
	p.wg.Add(1)
	p.runStartupTask(context.Background(), stale)
	if got := requestTokens(fake); fmt.Sprint(got) != "[token-b]" {
		t.Fatalf("startup used stale account selection: %v", got)
	}

	fake.mu.Lock()
	fake.requests = nil
	fake.mu.Unlock()
	recreated := stale
	recreated.CreatedAt = created.Add(time.Second)
	p.state.Tasks = []state.Task{recreated}
	p.wg.Add(1)
	p.runStartupTask(context.Background(), stale)
	if got := requestTokens(fake); len(got) != 0 {
		t.Fatalf("old generation timer ran a recreated task: %v", got)
	}

	p.state.Tasks = nil
	p.wg.Add(1)
	p.runStartupTask(context.Background(), stale)
	if got := requestTokens(fake); len(got) != 0 {
		t.Fatalf("deleted startup task still ran: %v", got)
	}
}

func TestTaskLockPreventsDuplicateRuns(t *testing.T) {
	fake := newRuntimeFakeHost()
	fake.delay = 50 * time.Millisecond
	p := newRuntimeForTest(t, fake)
	task := testTask("same", []string{"auth-a"})
	p.state.Tasks = []state.Task{task}
	results := make(chan state.RunRecord, 2)
	errorsCh := make(chan error, 2)
	for range 2 {
		go func() {
			record, err := p.executeTask(context.Background(), task, "schedule", nil, nil, true)
			results <- record
			errorsCh <- err
		}()
	}
	var statuses []string
	for range 2 {
		record := <-results
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, record.Status)
	}
	sort.Strings(statuses)
	if fmt.Sprint(statuses) != "[skipped success]" {
		t.Fatalf("duplicate task statuses = %v", statuses)
	}
	fake.mu.Lock()
	maxActive := fake.maxActive
	fake.mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("account request concurrency = %d", maxActive)
	}
}

func TestFinishRunReportsPersistFailure(t *testing.T) {
	fake := newRuntimeFakeHost()
	p := newRuntimeForTest(t, fake)
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := osWriteFile(parent); err != nil {
		t.Fatal(err)
	}
	p.statePath = filepath.Join(parent, "state.json")
	record := state.RunRecord{RunID: "run", TaskID: "task", Status: "failed", StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(), Results: []state.AccountResult{{AuthIndex: "auth-a", Status: "failed", Error: "failed"}}}
	if err := p.finishRun(record, testTask("task", nil), nil, p.cfg); err == nil {
		t.Fatal("finishRun unexpectedly hid persistence failure")
	}
}

func TestPersistFailureLogDoesNotDeadlockOnReentry(t *testing.T) {
	base := newRuntimeFakeHost()
	hostWithReentry := &reentrantLogHost{runtimeFakeHost: base}
	p := newRuntimeForTest(t, hostWithReentry)
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := osWriteFile(parent); err != nil {
		t.Fatal(err)
	}
	p.statePath = filepath.Join(parent, "state.json")
	var once sync.Once
	hostWithReentry.onLog = func() {
		once.Do(func() { _, _ = p.saveTasks([]byte(`{"tasks":[]}`)) })
	}
	record := state.RunRecord{RunID: "run", TaskID: "task", Status: "failed", StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(), Results: []state.AccountResult{{AuthIndex: "auth-a", Status: "failed", Error: "failed"}}}
	done := make(chan error, 1)
	go func() { done <- p.finishRun(record, testTask("task", nil), nil, p.cfg) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("finishRun unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("finishRun deadlocked during reentrant log")
	}
}

func TestRuntimeHistoryAndLogsDoNotExposeCredential(t *testing.T) {
	fake := newRuntimeFakeHost()
	secret := "token-a"
	fake.responses[secret] = host.HTTPResponse{StatusCode: 500, Body: []byte(`{"error":{"message":"Authorization: Bearer ` + secret + `"}}`)}
	p := newRuntimeForTest(t, fake)
	task := testTask("secret", []string{"auth-a"})
	p.state.Tasks = []state.Task{task}
	record, err := p.executeTask(context.Background(), task, "schedule", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded := fmt.Sprint(record)
	if strings.Contains(encoded, secret) {
		t.Fatalf("run record leaked credential: %s", encoded)
	}
	loaded, err := state.Load(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(loaded), secret) {
		t.Fatalf("saved history leaked credential: %#v", loaded)
	}
}

func TestShutdownWaitsForInFlightPersistence(t *testing.T) {
	fake := newRuntimeFakeHost()
	p := newRuntimeForTest(t, fake)
	p.persistMu.Lock()
	done := make(chan struct{})
	go func() {
		p.shutdown()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("shutdown did not wait for the persistence critical section")
	case <-time.After(10 * time.Millisecond):
	}
	p.persistMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not complete after persistence was released")
	}
	p.mu.RLock()
	closed, bridge := p.closed, p.host
	p.mu.RUnlock()
	if !closed || bridge != nil {
		t.Fatalf("shutdown state = closed:%t bridge:%#v", closed, bridge)
	}
}

func TestReconfigureWaitsForOldWakeAndDoesNotUseNewState(t *testing.T) {
	base := newRuntimeFakeHost()
	blocking := &blockingRuntimeHost{runtimeFakeHost: base, started: make(chan struct{}), release: make(chan struct{})}
	p := newRuntimeForTest(t, blocking)
	workdir := t.TempDir()
	oldPath := filepath.Join(workdir, "old.json")
	p.statePath = oldPath
	wakeDone := make(chan error, 1)
	go func() {
		_, err := p.wake([]byte(`{}`))
		wakeDone <- err
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("wake did not reach fake host")
	}
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(originalDir) }()
	configRequest, _ := json.Marshal(map[string]any{"config_yaml": "state_file: new.json\nenabled: true\nauto_wake: false\n"})
	reconfigureDone := make(chan error, 1)
	go func() { reconfigureDone <- p.configure(configRequest) }()
	select {
	case err := <-reconfigureDone:
		t.Fatalf("reconfigure completed before old wake ended: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(blocking.release)
	select {
	case err := <-wakeDone:
		if err != nil {
			t.Fatalf("old wake returned plugin error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old wake did not finish after fake host release")
	}
	select {
	case err := <-reconfigureDone:
		if err != nil {
			t.Fatalf("reconfigure error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconfigure did not finish after old wake release")
	}
	oldState, err := state.Load(oldPath)
	if err != nil || len(oldState.History) != 1 {
		t.Fatalf("old generation state = %#v, %v", oldState, err)
	}
	newState, err := state.Load(filepath.Join(workdir, "new.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(newState.History) != 0 || len(newState.Accounts) != 0 {
		t.Fatalf("old wake wrote new generation state = %#v", newState)
	}
	p.shutdown()
	response, err := p.wake([]byte(`{}`))
	if err != nil || response.StatusCode != 503 {
		t.Fatalf("wake after shutdown = %#v, %v", response, err)
	}
	saveResponse, err := p.saveTasks([]byte(`{"tasks":[]}`))
	if err != nil || saveResponse.StatusCode != 503 {
		t.Fatalf("saveTasks after shutdown = %#v, %v", saveResponse, err)
	}
}

func osWriteFile(path string) error {
	return os.WriteFile(path, []byte("file"), 0o600)
}
