package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/config"
	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/state"
)

// pluginRuntime owns all mutable plugin state. The lifecycle mutex serializes
// register/reconfigure/shutdown while mu protects state and configuration.
type pluginRuntime struct {
	lifecycleMu sync.Mutex
	mu          sync.RWMutex
	operationMu sync.Mutex

	host      host.Client
	cfg       config.Config
	state     state.State
	statePath string
	closed    bool
	scheduler schedulerDiagnostics

	cancel          context.CancelFunc
	wg              sync.WaitGroup
	persistMu       sync.Mutex
	operationCtx    context.Context
	operationCancel context.CancelFunc
	operationWG     sync.WaitGroup
	accepting       bool
	configured      bool

	taskLocks    map[string]*sync.Mutex
	accountLocks map[string]*sync.Mutex
	runSequence  uint64
}

func newPluginRuntime() *pluginRuntime {
	return &pluginRuntime{
		cfg:          config.Default(),
		state:        state.Empty(),
		taskLocks:    make(map[string]*sync.Mutex),
		accountLocks: make(map[string]*sync.Mutex),
	}
}

func (p *pluginRuntime) installHost(bridge *hostBridge) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.stopWorker()
	p.stopOperations()
	p.mu.Lock()
	old := p.host
	p.host = bridge
	p.closed = false
	configured := p.configured
	p.mu.Unlock()
	if old != nil && old != bridge {
		closeHostClient(old)
	}
	if configured && bridge != nil {
		p.startOperations()
	}
}

func (p *pluginRuntime) configure(raw []byte) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	cfg, err := parseRegistrationConfig(raw)
	if err != nil {
		return err
	}
	statePath, err := resolveStatePath(cfg.StateFile)
	if err != nil {
		return err
	}
	p.stopWorker()
	p.stopOperations()
	p.persistMu.Lock()
	loaded, loadErr := state.Load(statePath)
	if loadErr != nil {
		// A corrupt state file is quarantined by state.Load. Keep the plugin
		// usable with a fresh state and expose the condition in diagnostics.
		loaded = state.Empty()
	}
	loaded.Normalize(time.Now(), config.DefaultTaskInterval.String(), cfg.HistoryLimit)

	p.mu.Lock()
	p.cfg = cfg
	p.state = loaded
	p.statePath = statePath
	p.closed = false
	p.configured = true
	p.scheduler = schedulerDiagnostics{Phase: "stopped"}
	// state.Normalize cannot inspect the runtime quota cache. Rebuild quota
	// previews after loading so a restart does not temporarily erase a known
	// next reset until the first background scan.
	p.recomputeTaskNextRunsLocked(time.Now().In(time.Local))
	hasHost := p.host != nil
	shouldRun := cfg.Enabled && cfg.AutoWake && hasHost
	if shouldRun {
		p.startWorkerLocked()
	}
	p.mu.Unlock()
	if hasHost {
		p.startOperations()
	}
	p.persistMu.Unlock()
	if loadErr != nil {
		p.log("warn", "state file could not be loaded; using a fresh state", map[string]any{"error": scrubError(loadErr)})
	}
	p.log("info", "scheduler configured", map[string]any{
		"enabled": cfg.Enabled, "auto_wake": cfg.AutoWake, "worker_running": shouldRun,
		"scan_interval": cfg.ScanInterval.String(), "server_timezone": time.Now().Format("MST (UTC-07:00)"),
	})
	return nil
}

func parseRegistrationConfig(raw []byte) (config.Config, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return config.Parse(nil)
	}
	var request registrationRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return config.Config{}, fmt.Errorf("decode plugin registration request: %w", err)
	}
	configRaw := request.ConfigYAML
	if len(configRaw) == 0 || string(configRaw) == "null" {
		configRaw = request.Config
	}
	if len(configRaw) == 0 || string(configRaw) == "null" {
		return config.Parse(nil)
	}
	return config.Parse(decodeConfigYAML(configRaw))
}

// decodeConfigYAML accepts the current host wire shape (ConfigYAML []byte,
// encoded by encoding/json as base64) and the historical direct YAML string.
func decodeConfigYAML(raw json.RawMessage) []byte {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return raw
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	for _, decoder := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err := decoder.DecodeString(trimmed); err == nil && looksLikeConfigDocument(decoded) {
			return decoded
		}
	}
	return []byte(text)
}

func looksLikeConfigDocument(raw []byte) bool {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return false
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		return true
	}
	for _, key := range []string{"enabled", "auto_wake", "scan_interval", "state_file", "history_limit", "request_timeout", "default_model", "default_prompt", "default_max_output_tokens", "run_on_start", "upstream_url"} {
		if strings.HasPrefix(text, key+":") || strings.Contains(text, "\n"+key+":") {
			return true
		}
	}
	return strings.Contains(text, ":")
}

func resolveStatePath(relative string) (string, error) {
	if err := config.ValidateStateFile(relative); err != nil {
		return "", err
	}
	root, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve plugin working directory: %w", err)
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	return filepath.Clean(path), nil
}

func (p *pluginRuntime) startWorkerLocked() {
	if p.cancel != nil || p.host == nil || p.closed || !p.cfg.Enabled || !p.cfg.AutoWake {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Add(1)
	scanInterval := p.cfg.ScanInterval
	runOnStart := p.cfg.RunOnStart
	go p.worker(ctx, scanInterval, runOnStart)
	// A startup task is a schedule kind, independent of the legacy global
	// run_on_start switch. Each worker generation schedules it once; the
	// generation context and WaitGroup ensure reconfigure/shutdown cannot leave
	// a delayed task behind.
	for _, task := range p.state.Tasks {
		if !task.Enabled || state.NormalizeSchedule(task.Schedule, state.DefaultInterval).Kind != state.ScheduleKindStartup {
			continue
		}
		p.wg.Add(1)
		go p.runStartupTask(ctx, task)
	}
}

func (p *pluginRuntime) runStartupTask(ctx context.Context, task state.Task) {
	defer p.wg.Done()
	delay := time.Duration(task.Schedule.StartupDelayMinutes) * time.Minute
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return
	}
	// A delayed startup task may be edited, disabled, or deleted while it is
	// waiting. Re-check the current generation state immediately before the
	// request rather than executing the stale startup snapshot.
	p.mu.RLock()
	var currentTask state.Task
	for _, current := range p.state.Tasks {
		if current.ID == task.ID && current.CreatedAt.Equal(task.CreatedAt) && current.Enabled && state.NormalizeSchedule(current.Schedule, state.DefaultInterval).Kind == state.ScheduleKindStartup {
			currentTask = current
			break
		}
	}
	p.mu.RUnlock()
	if currentTask.ID == "" {
		return
	}
	// Account selection, prompt, model and enabled state may have changed while
	// the delay timer was running. Execute the current task, never the stale
	// startup snapshot. CreatedAt prevents a delete-and-recreate with the same ID
	// from inheriting an old generation's pending timer.
	_, _ = p.executeTask(ctx, currentTask, "startup", nil, nil, true)
}

func (p *pluginRuntime) stopWorker() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
}

// startOperations establishes the generation context used by management
// operations. The operation gate is separate from lifecycleMu so a stop can
// close admission and wait without holding the lifecycle lock while a host
// callback is returning.
func (p *pluginRuntime) startOperations() {
	p.operationMu.Lock()
	defer p.operationMu.Unlock()
	if p.accepting {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.operationCtx = ctx
	p.operationCancel = cancel
	p.accepting = true
}

func (p *pluginRuntime) stopOperations() {
	p.operationMu.Lock()
	p.accepting = false
	cancel := p.operationCancel
	p.operationCancel = nil
	p.operationCtx = nil
	p.operationMu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.operationWG.Wait()
}

func (p *pluginRuntime) beginOperation() (context.Context, func(), bool) {
	p.operationMu.Lock()
	defer p.operationMu.Unlock()
	if !p.accepting || p.operationCtx == nil {
		return nil, nil, false
	}
	p.operationWG.Add(1)
	ctx := p.operationCtx
	return ctx, func() { p.operationWG.Done() }, true
}

func (p *pluginRuntime) quiesce() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.stopWorker()
	p.stopOperations()
}

func (p *pluginRuntime) shutdown() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.stopWorker()
	p.stopOperations()
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	p.mu.Lock()
	p.closed = true
	bridge := p.host
	p.host = nil
	p.configured = false
	p.mu.Unlock()
	closeHostClient(bridge)
}

func (p *pluginRuntime) worker(ctx context.Context, interval time.Duration, runOnStart bool) {
	defer p.wg.Done()
	p.mu.Lock()
	p.scheduler.StartedAt = timePtr(time.Now().UTC())
	p.mu.Unlock()
	p.log("info", "scheduler started", map[string]any{"scan_interval": interval.String()})
	defer func() {
		p.setScanPhase("stopped", "")
		p.log("info", "scheduler stopped", nil)
	}()
	// Already-due tasks should not wait another scan interval after a restart.
	// run_on_start only controls the extra run of never-run, not-yet-due tasks.
	p.runDue(ctx, runOnStart)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.runDue(ctx, false)
		}
	}
}

func (p *pluginRuntime) runDue(ctx context.Context, includeStartup bool) {
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	p.scheduler.LastScanAt = timePtr(time.Now().UTC())
	p.scheduler.ScanCount++
	p.scheduler.Phase = "quota_refresh"
	p.scheduler.CurrentTaskID = ""
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.scheduler.LastScanCompletedAt = timePtr(time.Now().UTC())
		p.scheduler.Phase = "idle"
		p.scheduler.CurrentTaskID = ""
		p.mu.Unlock()
	}()
	p.mu.RLock()
	tasks := append([]state.Task(nil), p.state.Tasks...)
	runOnStart := p.cfg.RunOnStart
	p.mu.RUnlock()
	quotaEntries := p.refreshQuotaForTasks(ctx, tasks)
	// A persisted quota cache is not proof the account still exists or is
	// enabled. Require this scan's successful host listing as well.
	liveQuotaAccounts := make(map[string]bool, len(quotaEntries))
	for _, entry := range quotaEntries {
		liveQuotaAccounts[entry.AuthIndex] = true
	}
	// A refresh may take time; pick up edits and completed manual runs before
	// deciding which tasks are due, rather than using the pre-refresh snapshot.
	p.mu.RLock()
	tasks = append([]state.Task(nil), p.state.Tasks...)
	p.mu.RUnlock()
	p.setScanPhase("evaluating", "")
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		p.setScanPhase("evaluating", task.ID)
		kind := state.NormalizeSchedule(task.Schedule, state.DefaultInterval).Kind
		now := time.Now().UTC()
		if kind == state.ScheduleKindStartup {
			// Startup tasks are scheduled by startWorkerLocked so delayed tasks
			// can coexist with the regular scan loop and run exactly once.
			continue
		}
		if kind == state.ScheduleKindQuotaReset {
			accounts := p.quotaDueAccounts(task, now)
			filtered := accounts[:0]
			for _, id := range accounts {
				if liveQuotaAccounts[id] {
					filtered = append(filtered, id)
				}
			}
			accounts = filtered
			if len(accounts) == 0 {
				p.logTaskNotDue(task)
				continue
			}
			p.setScanPhase("executing", task.ID)
			_, _ = p.executeTask(ctx, task, "quota_reset", accounts, nil, true)
			continue
		}
		if !state.Due(task, now, includeStartup && runOnStart) {
			p.logTaskNotDue(task)
			continue
		}
		p.setScanPhase("executing", task.ID)
		_, _ = p.executeTask(ctx, task, "schedule", nil, nil, true)
	}
}

// recomputeTaskNextRunsLocked updates persisted previews from the current
// in-memory account cache. The caller must hold p.mu for writing.
func (p *pluginRuntime) recomputeTaskNextRunsLocked(now time.Time) {
	for index := range p.state.Tasks {
		task := &p.state.Tasks[index]
		switch state.NormalizeSchedule(task.Schedule, state.DefaultInterval).Kind {
		case state.ScheduleKindStartup:
			task.NextRunAt = time.Time{}
		case state.ScheduleKindQuotaReset:
			task.NextRunAt = p.quotaNextRunAtLocked(*task, now)
		default:
			if task.NextRunAt.IsZero() {
				task.NextRunAt = state.NextRunAt(*task, now, nil)
			}
		}
	}
}

func (p *pluginRuntime) taskLock(id string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lock := p.taskLocks[id]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	p.taskLocks[id] = lock
	return lock
}

func (p *pluginRuntime) accountLock(id string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lock := p.accountLocks[id]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	p.accountLocks[id] = lock
	return lock
}

type accountCandidate struct {
	entry host.AuthFile
	key   string
}

func (p *pluginRuntime) executeTask(ctx context.Context, task state.Task, trigger string, selected []string, override *wakeSettings, persistTask bool) (record state.RunRecord, err error) {
	started := time.Now().UTC()
	runID := fmt.Sprintf("%d-%d", started.UnixNano(), atomic.AddUint64(&p.runSequence, 1))
	record = state.RunRecord{RunID: runID, TaskID: task.ID, Trigger: trigger, StartedAt: started}
	defer func() { p.logRunResult(record, err) }()
	lock := p.taskLock(task.ID)
	if task.ID != "" && !lock.TryLock() {
		record.Status = "skipped"
		record.CompletedAt = time.Now().UTC()
		record.Results = []state.AccountResult{{Status: "skipped", Error: "task is already running"}}
		return record, nil
	}
	if task.ID != "" {
		defer lock.Unlock()
	}

	p.mu.RLock()
	bridge := p.host
	cfg := p.cfg
	closed := p.closed
	p.mu.RUnlock()
	if closed || bridge == nil {
		return record, errors.New("plugin host is unavailable")
	}
	if ctx.Err() != nil {
		return record, ctx.Err()
	}
	p.log("info", "task run started", map[string]any{
		"run_id": runID, "task_id": sanitizeText(task.ID), "trigger": trigger, "scheduled_at": task.NextRunAt,
	})

	entries, err := bridge.ListAuthFiles(ctx)
	if err != nil {
		record.Status = "failed"
		record.CompletedAt = time.Now().UTC()
		record.Results = []state.AccountResult{{Status: "failed", Error: scrubError(err)}}
		if persistTask {
			return record, p.finishRun(record, task, nil, cfg)
		}
		return record, nil
	}
	predicate := eligibleAuth
	if trigger == "quota_reset" {
		// Another scan snapshot or a management edit may be stale by the
		// time host listing returns. Recheck the current task under its run
		// lock before allowing a recovery probe past host unavailability.
		p.mu.RLock()
		var current state.Task
		for _, saved := range p.state.Tasks {
			if saved.ID == task.ID && saved.CreatedAt.Equal(task.CreatedAt) && saved.Enabled && state.NormalizeSchedule(saved.Schedule, state.DefaultInterval).Kind == state.ScheduleKindQuotaReset {
				current = saved
				break
			}
		}
		p.mu.RUnlock()
		if current.ID == "" {
			record.Status = "skipped"
			record.CompletedAt = time.Now().UTC()
			record.Results = []state.AccountResult{{Status: "skipped", Error: "quota task was disabled, changed or deleted"}}
			return record, nil
		}
		task = current
		due := make(map[string]bool)
		for _, id := range p.quotaDueAccounts(task, started) {
			due[id] = true
		}
		predicate = func(entry host.AuthFile) bool {
			return monitorableAuth(entry) && due[strings.TrimSpace(entry.AuthIndex)]
		}
	}
	candidates := selectCandidatesMatching(entries, predicate, selected, task.AccountIDs)
	if len(candidates) == 0 {
		record.Status = "skipped"
		record.CompletedAt = time.Now().UTC()
		record.Results = []state.AccountResult{{Status: "skipped", Error: "no eligible Codex OAuth accounts"}}
		if persistTask {
			return record, p.finishRun(record, task, nil, cfg)
		}
		return record, nil
	}
	settings := settingsForTask(cfg, task, override)
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		accountLock := p.accountLock(candidate.key)
		if !accountLock.TryLock() {
			record.Results = append(record.Results, state.AccountResult{
				AuthIndex: candidate.entry.AuthIndex,
				Label:     safeAccountLabel(candidate.entry),
				Status:    "skipped",
				Error:     "account is already running",
			})
			continue
		}
		result := p.executeAccount(ctx, bridge, candidate.entry, settings)
		accountLock.Unlock()
		record.Results = append(record.Results, result)
	}
	record.CompletedAt = time.Now().UTC()
	record.Status = aggregateRunStatus(record.Results)
	if persistTask {
		return record, p.finishRun(record, task, candidates, cfg)
	}
	return record, p.appendAdHocRun(record, cfg.HistoryLimit)
}

func selectCandidates(entries []host.AuthFile, selected ...[]string) []accountCandidate {
	return selectCandidatesMatching(entries, eligibleAuth, selected...)
}

func selectCandidatesMatching(entries []host.AuthFile, eligible func(host.AuthFile) bool, selected ...[]string) []accountCandidate {
	var selectors []string
	for _, item := range selected {
		if item != nil {
			selectors = item
			break
		}
	}
	selectedSet := make(map[string]struct{}, len(selectors))
	for _, value := range selectors {
		value = strings.TrimSpace(value)
		if value != "" {
			selectedSet[value] = struct{}{}
		}
	}
	result := make([]accountCandidate, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if !eligible(entry) {
			continue
		}
		if len(selectedSet) != 0 && !matchesAuth(entry, selectedSet) {
			continue
		}
		key := strings.TrimSpace(entry.AuthIndex)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, accountCandidate{entry: entry, key: key})
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].entry.AuthIndex < result[j].entry.AuthIndex
	})
	return result
}

func matchesAuth(entry host.AuthFile, selected map[string]struct{}) bool {
	// AuthIndex is the only selector. IDs can be paths or mutable host values.
	if _, ok := selected[strings.TrimSpace(entry.AuthIndex)]; ok {
		return true
	}
	return false
}

func settingsForTask(cfg config.Config, task state.Task, override *wakeSettings) wakeSettings {
	settings := wakeSettings{
		Model:           firstNonEmpty(task.Model, cfg.DefaultModel),
		Prompt:          firstNonEmpty(task.Prompt, cfg.DefaultPrompt),
		MaxOutputTokens: task.MaxOutputTokens,
		UpstreamURL:     cfg.UpstreamURL,
		Timeout:         cfg.RequestTimeout,
	}
	if settings.MaxOutputTokens < 1 {
		settings.MaxOutputTokens = cfg.DefaultMaxTokens
	}
	if override != nil {
		if override.Model != "" {
			settings.Model = override.Model
		}
		if override.Prompt != "" {
			settings.Prompt = override.Prompt
		}
		if override.MaxOutputTokens > 0 {
			settings.MaxOutputTokens = override.MaxOutputTokens
		}
		if override.Timeout > 0 {
			settings.Timeout = override.Timeout
		}
	}
	return settings
}

func (p *pluginRuntime) executeAccount(parent context.Context, bridge host.Client, entry host.AuthFile, settings wakeSettings) state.AccountResult {
	started := time.Now()
	result := state.AccountResult{
		AuthIndex: entry.AuthIndex,
		Label:     safeAccountLabel(entry),
		Status:    "failed",
	}
	authFile, err := bridge.GetAuthFile(parent, entry.AuthIndex)
	if err != nil {
		result.Error = scrubError(err)
		result.DurationMS = time.Since(started).Milliseconds()
		return result
	}
	if authFile.Disabled || strings.EqualFold(strings.TrimSpace(authFile.Status), "disabled") {
		result.Status = "skipped"
		result.Error = "account was disabled before execution"
		result.DurationMS = time.Since(started).Milliseconds()
		return result
	}
	credential, err := parseCodexCredential(authFile.RawJSON)
	if err != nil {
		result.Error = scrubError(formatAuthError(entry, err))
		result.DurationMS = time.Since(started).Milliseconds()
		return result
	}
	requestContext, cancel := context.WithTimeout(parent, settings.Timeout)
	defer cancel()
	codex, err := executeCodex(requestContext, bridge, entry, credential, settings)
	result.HTTPStatus = codex.HTTPStatus
	result.InputTokens = codex.InputTokens
	result.OutputTokens = codex.OutputTokens
	result.TotalTokens = codex.TotalTokens
	result.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = scrubError(formatAuthError(entry, err))
		return result
	}
	result.Status = "success"
	return result
}

func safeAccountLabel(entry host.AuthFile) string {
	label := firstNonEmpty(entry.Label, entry.Name, entry.Email, entry.AuthIndex)
	if entry.Email != "" {
		return maskEmail(entry.Email)
	}
	return sanitizeText(label)
}

func aggregateRunStatus(results []state.AccountResult) string {
	if len(results) == 0 {
		return "skipped"
	}
	successes, failures, skipped := 0, 0, 0
	for _, result := range results {
		switch result.Status {
		case "success":
			successes++
		case "skipped":
			skipped++
		default:
			failures++
		}
	}
	switch {
	case successes > 0 && failures == 0 && skipped == 0:
		return "success"
	case successes > 0:
		return "partial"
	case failures > 0:
		return "failed"
	default:
		return "skipped"
	}
}

func (p *pluginRuntime) finishRun(record state.RunRecord, task state.Task, candidates []accountCandidate, cfg config.Config) error {
	p.persistMu.Lock()
	p.mu.Lock()
	p.applyAccountResultsLocked(record)
	for index := range p.state.Tasks {
		if p.state.Tasks[index].ID != task.ID {
			continue
		}
		now := record.CompletedAt
		if state.NormalizeSchedule(p.state.Tasks[index].Schedule, state.DefaultInterval).Kind == state.ScheduleKindQuotaReset {
			p.markQuotaResetsHandledLocked(&p.state.Tasks[index], record)
		}
		p.state.Tasks[index].LastRunAt = &now
		p.state.Tasks[index].LastStatus = record.Status
		for _, result := range record.Results {
			if result.Status == "success" {
				p.state.Tasks[index].SuccessCount++
			} else if result.Status == "failed" {
				p.state.Tasks[index].FailureCount++
			}
		}
		updated := &p.state.Tasks[index]
		switch state.NormalizeSchedule(updated.Schedule, state.DefaultInterval).Kind {
		case state.ScheduleKindStartup:
			// Startup is a one-shot per plugin generation. A zero next_run_at
			// makes that semantic visible after the task has fired.
			updated.NextRunAt = time.Time{}
		case state.ScheduleKindQuotaReset:
			updated.NextRunAt = p.quotaNextRunAtLocked(*updated, now)
		default:
			updated.NextRunAt = state.NextRunAt(*updated, now.In(time.Local), nil)
		}
		break
	}
	state.AppendHistory(&p.state, record, cfg.HistoryLimit)
	snapshot := p.state.Clone()
	path := p.statePath
	p.mu.Unlock()
	err := p.persistSnapshot(path, snapshot, cfg.HistoryLimit)
	p.persistMu.Unlock()
	if record.Trigger == "quota_reset" {
		for _, saved := range snapshot.Tasks {
			if saved.ID != record.TaskID {
				continue
			}
			for _, result := range record.Results {
				if retry, ok := saved.QuotaRetries[result.AuthIndex]; ok && result.Status == "failed" {
					p.log("warn", "quota wake retry scheduled", map[string]any{
						"task_id": sanitizeText(saved.ID), "auth_index": sanitizeText(result.AuthIndex),
						"reset_at": retry.ResetAt, "next_retry_at": retry.NextRetryAt, "failures": retry.Failures,
					})
				}
			}
		}
	}
	if err != nil {
		p.log("warn", "state save failed", map[string]any{"error": scrubError(err)})
	}
	_ = candidates
	return err
}

func (p *pluginRuntime) applyAccountResultsLocked(record state.RunRecord) {
	for _, result := range record.Results {
		if result.AuthIndex == "" {
			continue
		}
		account := p.state.Accounts[result.AuthIndex]
		account.AuthIndex = result.AuthIndex
		account.Label = result.Label
		account.LastRunAt = record.CompletedAt
		account.LastStatus = result.Status
		account.LastError = sanitizeText(result.Error)
		account.LastHTTPStatus = result.HTTPStatus
		account.LastDurationMS = result.DurationMS
		account.InputTokens += result.InputTokens
		account.OutputTokens += result.OutputTokens
		account.TotalTokens += result.TotalTokens
		if result.Status == "success" {
			account.SuccessCount++
		} else if result.Status == "failed" {
			account.FailureCount++
		}
		p.state.Accounts[result.AuthIndex] = account
	}
}

func (p *pluginRuntime) appendAdHocRun(record state.RunRecord, historyLimit int) error {
	p.persistMu.Lock()
	p.mu.Lock()
	p.applyAccountResultsLocked(record)
	state.AppendHistory(&p.state, record, historyLimit)
	snapshot := p.state.Clone()
	path := p.statePath
	p.mu.Unlock()
	err := p.persistSnapshot(path, snapshot, historyLimit)
	p.persistMu.Unlock()
	if err != nil {
		p.log("warn", "state save failed", map[string]any{"error": scrubError(err)})
	}
	return err
}

func (p *pluginRuntime) persistSnapshot(path string, snapshot state.State, historyLimit int) error {
	if path == "" {
		return nil
	}
	return state.Save(path, snapshot, historyLimit)
}

func (p *pluginRuntime) stateSnapshot() (state.State, config.Config, string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state.Clone(), p.cfg, p.statePath, p.cancel != nil && !p.closed
}

func (p *pluginRuntime) log(level, message string, fields map[string]any) {
	p.mu.Lock()
	bridge := p.host
	if value, ok := fields["error"].(string); ok && value != "" {
		p.scheduler.LastError = sanitizeText(value)
	}
	p.mu.Unlock()
	if bridge != nil {
		bridge.Log(context.Background(), level, pluginName+": "+message, fields)
	}
}

func (p *pluginRuntime) handleManagementRPC(raw []byte) ([]byte, error) {
	var request managementRequest
	if len(strings.TrimSpace(string(raw))) != 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
	}
	response, err := p.handleManagement(request)
	if err != nil {
		return nil, err
	}
	return okEnvelope(response)
}
