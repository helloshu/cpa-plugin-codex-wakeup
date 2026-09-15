package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/config"
	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/state"
)

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes"`
	Resources []resourceRoute   `json:"resources"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers,omitempty"`
	Query   map[string][]string `json:"query,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

type managementResponse struct {
	// These keys intentionally match pluginapi.ManagementResponse, whose
	// fields have no JSON tags in CLIProxyAPI v7.3.3. Body is []byte and is
	// therefore base64-encoded by encoding/json on the RPC wire.
	StatusCode int                 `json:"StatusCode,omitempty"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

const managementNamespace = "/codex-wakeup"

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: managementNamespace + "/overview", Description: "查看唤醒插件概览。"},
			{Method: http.MethodGet, Path: managementNamespace + "/accounts", Description: "查看可唤醒的 Codex OAuth 账号。"},
			{Method: http.MethodPost, Path: managementNamespace + "/preview", Description: "按服务器本地时区预览任务的后续触发时间。"},
			{Method: http.MethodGet, Path: managementNamespace + "/tasks", Description: "查看唤醒任务。"},
			{Method: http.MethodPost, Path: managementNamespace + "/tasks", Description: "新增或替换一个唤醒任务。"},
			{Method: http.MethodPut, Path: managementNamespace + "/tasks", Description: "更新一个唤醒任务。"},
			{Method: http.MethodPatch, Path: managementNamespace + "/tasks", Description: "启用或停用一个唤醒任务。"},
			{Method: http.MethodDelete, Path: managementNamespace + "/tasks", Description: "删除一个唤醒任务。"},
			{Method: http.MethodPost, Path: managementNamespace + "/save-tasks", Description: "保存唤醒任务。"},
			{Method: http.MethodPost, Path: managementNamespace + "/wake", Description: "手动唤醒账号或任务。"},
			{Method: http.MethodGet, Path: managementNamespace + "/history", Description: "查看唤醒历史。"},
			{Method: http.MethodGet, Path: managementNamespace + "/diagnostics", Description: "查看脱敏诊断信息。"},
		},
		Resources: []resourceRoute{
			{Path: "/status", Menu: "Codex 唤醒", Description: "Codex 唤醒任务管理页面。"},
		},
	}
}

func (p *pluginRuntime) handleManagement(request managementRequest) (managementResponse, error) {
	path := normalizeManagementPath(request.Path)
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	switch {
	case method == http.MethodGet && path == "/status":
		return p.statusResource(), nil
	case method == http.MethodGet && path == "/overview":
		return p.jsonManagementResponse(p.overview())
	case method == http.MethodGet && path == "/accounts":
		return p.accountsResponse()
	case method == http.MethodPost && path == "/preview":
		return p.previewTask(request.Body)
	case method == http.MethodGet && path == "/tasks":
		stateValue, _, _, _ := p.stateSnapshot()
		return p.jsonManagementResponse(map[string]any{"tasks": managementTasks(stateValue.Tasks)})
	case (method == http.MethodPost || method == http.MethodPut) && path == "/tasks":
		return p.saveSingleTask(request.Body, method == http.MethodPut)
	case method == http.MethodPatch && path == "/tasks":
		return p.toggleTask(request.Body)
	case method == http.MethodDelete && path == "/tasks":
		return p.deleteTask(request.Body)
	case method == http.MethodPost && path == "/save-tasks":
		return p.saveTasks(request.Body)
	case method == http.MethodPost && path == "/wake":
		return p.wake(request.Body)
	case method == http.MethodGet && path == "/history":
		return p.historyResponse(request.Query)
	case method == http.MethodGet && path == "/diagnostics":
		return p.jsonManagementResponse(p.diagnostics())
	default:
		return managementJSONError(http.StatusNotFound, "not_found", "未知的插件管理路径"), nil
	}
}

func normalizeManagementPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	for _, prefix := range []string{"/v0/management" + managementNamespace, managementNamespace, "/v0/resource/plugins/codex-wakeup"} {
		if path == prefix {
			return "/"
		}
		if strings.HasPrefix(path, prefix+"/") {
			return strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/")
		}
	}
	// Keep direct relative paths useful for unit tests and older callers.
	if strings.HasPrefix(path, "/v0/management/") {
		path = strings.TrimPrefix(path, "/v0/management")
	}
	return path
}

func (p *pluginRuntime) jsonManagementResponse(value any) (managementResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return managementResponse{}, err
	}
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}},
		Body:       body,
	}, nil
}

func managementJSONError(status int, code, message string) managementResponse {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return managementResponse{StatusCode: status, Headers: map[string][]string{"Content-Type": {"application/json; charset=utf-8"}}, Body: body}
}

func (p *pluginRuntime) overview() map[string]any {
	stateValue, cfg, path, running := p.stateSnapshot()
	now := time.Now()
	return map[string]any{
		"plugin": pluginName, "version": pluginVersion, "enabled": cfg.Enabled, "auto_wake": cfg.AutoWake,
		"worker_running": running, "task_count": len(stateValue.Tasks), "history_count": len(stateValue.History),
		"state_file": relativeStatePath(path), "default_model": cfg.DefaultModel,
		"scan_interval": cfg.ScanInterval.String(), "request_timeout": cfg.RequestTimeout.String(),
		"default_max_output_tokens": cfg.DefaultMaxTokens, "secrets_logged": false,
		"server_local_time": now.Format("2006-01-02 15:04:05 -07:00"),
		"server_timezone":   now.Format("MST (UTC-07:00)"),
	}
}

type previewTaskRequest struct {
	Task state.Task `json:"task"`
}

func (p *pluginRuntime) previewTask(body []byte) (managementResponse, error) {
	if len(body) > 64<<10 {
		return managementJSONError(http.StatusRequestEntityTooLarge, "request_too_large", "任务数据过大"), nil
	}
	var request previewTaskRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return managementJSONError(http.StatusBadRequest, "invalid_task", "任务 JSON 无效"), nil
	}
	task := request.Task
	if err := state.ValidateSchedule(task.Schedule); err != nil {
		return managementJSONError(http.StatusBadRequest, "invalid_schedule", scrubError(err)), nil
	}
	task.Schedule = state.NormalizeSchedule(task.Schedule, config.DefaultTaskInterval.String())
	for _, accountID := range uniqueNonEmpty(task.AccountIDs) {
		if !validSafeID(accountID, 256) {
			return managementJSONError(http.StatusBadRequest, "invalid_account_id", "账号必须使用有效的 auth_index"), nil
		}
	}

	now := time.Now().In(time.Local)
	task.CreatedAt = now
	task.LastRunAt = nil
	task.NextRunAt = time.Time{}
	response := map[string]any{
		"preview":           []string{},
		"server_local_time": now.Format("2006-01-02 15:04:05 -07:00"),
		"server_timezone":   now.Format("MST (UTC-07:00)"),
	}
	if task.Schedule.Kind == state.ScheduleKindStartup {
		response["message"] = fmt.Sprintf("插件每次启动后延迟 %d 分钟执行一次", task.Schedule.StartupDelayMinutes)
		return p.jsonManagementResponse(response)
	}

	var quotaResets []time.Time
	if task.Schedule.Kind == state.ScheduleKindQuotaReset {
		p.mu.RLock()
		quotaResets = append(quotaResets, p.quotaTimesForTaskLocked(task)...)
		p.mu.RUnlock()
	}
	preview := make([]string, 0, 3)
	cursor := now
	for len(preview) < 3 {
		next := state.NextRunAt(task, cursor, quotaResets)
		if next.IsZero() {
			break
		}
		preview = append(preview, next.UTC().Format(time.RFC3339))
		last := next
		if task.Schedule.Kind == state.ScheduleKindQuotaReset {
			last = next.Add(-state.QuotaResetDelay)
		}
		task.LastRunAt = &last
		cursor = next.In(time.Local).Add(time.Nanosecond)
	}
	response["preview"] = preview
	if len(preview) == 0 && task.Schedule.Kind == state.ScheduleKindQuotaReset {
		response["message"] = "等待官方 usage 查询返回真实 reset_at"
	}
	return p.jsonManagementResponse(response)
}

func relativeStatePath(path string) string {
	if path == "" {
		return ""
	}
	if cwd, err := os.Getwd(); err == nil {
		if relative, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(relative, "..") {
			return filepath.ToSlash(relative)
		}
	}
	return "configured relative state file"
}

type managementAccount struct {
	AuthIndex            string `json:"auth_index"`
	Name                 string `json:"name,omitempty"`
	Label                string `json:"label,omitempty"`
	Email                string `json:"email,omitempty"`
	Provider             string `json:"provider,omitempty"`
	Status               string `json:"status,omitempty"`
	HostUnavailable      bool   `json:"host_unavailable"`
	WakeAvailable        bool   `json:"wake_available"`
	QuotaMonitoring      bool   `json:"quota_monitoring"`
	LastRunAt            string `json:"last_run_at,omitempty"`
	LastStatus           string `json:"last_status,omitempty"`
	Successes            int64  `json:"success_count"`
	Failures             int64  `json:"failure_count"`
	LastError            string `json:"last_error,omitempty"`
	LastHTTPStatus       int    `json:"last_http_status,omitempty"`
	LastDurationMS       int64  `json:"last_duration_ms,omitempty"`
	PrimaryResetAt       string `json:"primary_reset_at,omitempty"`
	SecondaryResetAt     string `json:"secondary_reset_at,omitempty"`
	QuotaLastRefreshAt   string `json:"quota_last_refresh_at,omitempty"`
	QuotaNextRefreshAt   string `json:"quota_next_refresh_at,omitempty"`
	QuotaLastError       string `json:"quota_last_error,omitempty"`
	QuotaRefreshFailures int    `json:"quota_refresh_failures,omitempty"`
}

func (p *pluginRuntime) accountsResponse() (managementResponse, error) {
	p.mu.RLock()
	bridge := p.host
	// Do not retain the live map after releasing p.mu: quota refreshes and
	// task runs update account state concurrently with this read path.
	cached := make(map[string]state.AccountState, len(p.state.Accounts))
	for key, value := range p.state.Accounts {
		cached[key] = value
	}
	monitored := make(map[string]bool)
	monitorAll := false
	if p.cfg.Enabled && p.cfg.AutoWake {
		for _, task := range p.state.Tasks {
			if task.Enabled && state.NormalizeSchedule(task.Schedule, state.DefaultInterval).Kind == state.ScheduleKindQuotaReset {
				if len(task.AccountIDs) == 0 {
					monitorAll = true
				}
				for _, id := range task.AccountIDs {
					monitored[id] = true
				}
			}
		}
	}
	p.mu.RUnlock()
	if bridge == nil {
		return p.jsonManagementResponse(map[string]any{"accounts": []managementAccount{}, "error": "host unavailable"})
	}
	entries, err := bridge.ListAuthFiles(context.Background())
	if err != nil {
		return managementJSONError(http.StatusServiceUnavailable, "host_unavailable", scrubError(err)), nil
	}
	accounts := make([]managementAccount, 0, len(entries))
	for _, entry := range entries {
		if !monitorableAuth(entry) {
			continue
		}
		item := managementAccount{AuthIndex: entry.AuthIndex, Name: sanitizeText(entry.Name), Label: sanitizeText(entry.Label), Email: maskEmail(entry.Email), Provider: sanitizeText(entry.Provider), Status: sanitizeText(entry.Status)}
		item.HostUnavailable = !eligibleAuth(entry)
		item.WakeAvailable = eligibleAuth(entry)
		item.QuotaMonitoring = monitorAll || monitored[entry.AuthIndex]
		if previous, ok := cached[entry.AuthIndex]; ok {
			if !previous.LastRunAt.IsZero() {
				item.LastRunAt = previous.LastRunAt.Format(time.RFC3339)
			}
			item.LastStatus = sanitizeText(previous.LastStatus)
			item.Successes = previous.SuccessCount
			item.Failures = previous.FailureCount
			item.LastError = sanitizeText(previous.LastError)
			item.LastHTTPStatus = previous.LastHTTPStatus
			item.LastDurationMS = previous.LastDurationMS
			if previous.PrimaryResetAt != nil && !previous.PrimaryResetAt.IsZero() {
				item.PrimaryResetAt = previous.PrimaryResetAt.UTC().Format(time.RFC3339)
			}
			if previous.SecondaryResetAt != nil && !previous.SecondaryResetAt.IsZero() {
				item.SecondaryResetAt = previous.SecondaryResetAt.UTC().Format(time.RFC3339)
			}
			if previous.QuotaLastRefreshAt != nil && !previous.QuotaLastRefreshAt.IsZero() {
				item.QuotaLastRefreshAt = previous.QuotaLastRefreshAt.UTC().Format(time.RFC3339)
			}
			if previous.QuotaNextRefreshAt != nil && !previous.QuotaNextRefreshAt.IsZero() {
				item.QuotaNextRefreshAt = previous.QuotaNextRefreshAt.UTC().Format(time.RFC3339)
			}
			item.QuotaLastError = sanitizeText(previous.QuotaLastError)
			item.QuotaRefreshFailures = previous.QuotaRefreshFailures
		}
		accounts = append(accounts, item)
	}
	return p.jsonManagementResponse(map[string]any{"accounts": accounts})
}

type saveTasksRequest struct {
	Tasks []state.Task `json:"tasks"`
}

type singleTaskRequest struct {
	Task    state.Task `json:"task"`
	ID      string     `json:"id,omitempty"`
	Enabled *bool      `json:"enabled,omitempty"`
}

func (p *pluginRuntime) saveSingleTask(body []byte, requireExisting bool) (managementResponse, error) {
	if len(body) > 256<<10 {
		return managementJSONError(http.StatusRequestEntityTooLarge, "request_too_large", "任务数据过大"), nil
	}
	var envelope singleTaskRequest
	if err := json.Unmarshal(body, &envelope); err != nil {
		return managementJSONError(http.StatusBadRequest, "invalid_task", "任务 JSON 无效"), nil
	}
	task := envelope.Task
	// Accept both the documented {"task": {...}} envelope and a direct task
	// object for older management clients.
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) == nil {
		if _, hasTask := fields["task"]; !hasTask {
			var direct state.Task
			if json.Unmarshal(body, &direct) == nil {
				task = direct
			}
		}
	}
	if task.ID == "" {
		task.ID = strings.TrimSpace(envelope.ID)
	}
	if requireExisting && task.ID == "" {
		return managementJSONError(http.StatusBadRequest, "task_id_required", "更新任务必须提供 ID"), nil
	}
	if envelope.Enabled != nil {
		task.Enabled = *envelope.Enabled
	}
	// The form-based CRUD endpoint always carries an explicit account
	// selection. Keep the older bulk save-tasks endpoint permissive so legacy
	// state using an empty account_ids list (meaning all eligible accounts)
	// remains loadable and editable through that API.
	if len(uniqueNonEmpty(task.AccountIDs)) == 0 {
		return managementJSONError(http.StatusBadRequest, "accounts_required", "请至少选择一个 Codex OAuth 账号"), nil
	}
	stateValue, _, _, _ := p.stateSnapshot()
	if !requireExisting && task.ID == "" {
		task.ID = fmt.Sprintf("task-%d", time.Now().UTC().UnixNano())
	}
	if requireExisting {
		found := false
		for _, existing := range stateValue.Tasks {
			if existing.ID == task.ID {
				found = true
				break
			}
		}
		if !found {
			return managementJSONError(http.StatusNotFound, "task_not_found", "任务不存在"), nil
		}
	}
	if requireExisting {
		for index := range stateValue.Tasks {
			if stateValue.Tasks[index].ID == task.ID {
				stateValue.Tasks[index] = task
				break
			}
		}
	} else {
		stateValue.Tasks = append(stateValue.Tasks, task)
	}
	encoded, err := json.Marshal(saveTasksRequest{Tasks: stateValue.Tasks})
	if err != nil {
		return managementJSONError(http.StatusInternalServerError, "encode_task", "任务编码失败"), nil
	}
	return p.saveTasks(encoded)
}

func (p *pluginRuntime) toggleTask(body []byte) (managementResponse, error) {
	var request struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled,omitempty"`
	}
	if len(body) > 16<<10 || json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.ID) == "" {
		return managementJSONError(http.StatusBadRequest, "invalid_task", "任务 ID 无效"), nil
	}
	stateValue, _, _, _ := p.stateSnapshot()
	for index := range stateValue.Tasks {
		if stateValue.Tasks[index].ID != strings.TrimSpace(request.ID) {
			continue
		}
		enabled := !stateValue.Tasks[index].Enabled
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		stateValue.Tasks[index].Enabled = enabled
		encoded, _ := json.Marshal(saveTasksRequest{Tasks: stateValue.Tasks})
		return p.saveTasks(encoded)
	}
	return managementJSONError(http.StatusNotFound, "task_not_found", "任务不存在"), nil
}

func (p *pluginRuntime) deleteTask(body []byte) (managementResponse, error) {
	var request struct {
		ID string `json:"id"`
	}
	if len(body) > 16<<10 || json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.ID) == "" {
		return managementJSONError(http.StatusBadRequest, "invalid_task", "任务 ID 无效"), nil
	}
	id := strings.TrimSpace(request.ID)
	stateValue, _, _, _ := p.stateSnapshot()
	filtered := make([]state.Task, 0, len(stateValue.Tasks))
	found := false
	for _, task := range stateValue.Tasks {
		if task.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, task)
	}
	if !found {
		return managementJSONError(http.StatusNotFound, "task_not_found", "任务不存在"), nil
	}
	encoded, _ := json.Marshal(saveTasksRequest{Tasks: filtered})
	return p.saveTasks(encoded)
}

func validSafeID(value string, max int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max {
		return false
	}
	return !strings.ContainsAny(value, "\r\n") && !strings.ContainsFunc(value, unicode.IsControl)
}

func schedulesEquivalent(a, b state.Schedule) bool {
	a = state.NormalizeSchedule(a, state.DefaultInterval)
	b = state.NormalizeSchedule(b, state.DefaultInterval)
	if a.Kind != b.Kind || a.Interval != b.Interval || a.DailyTime != b.DailyTime || a.WeeklyTime != b.WeeklyTime || a.QuotaResetWindow != b.QuotaResetWindow || a.StartupDelayMinutes != b.StartupDelayMinutes || len(a.WeeklyDays) != len(b.WeeklyDays) {
		return false
	}
	for index := range a.WeeklyDays {
		if a.WeeklyDays[index] != b.WeeklyDays[index] {
			return false
		}
	}
	return true
}

func managementTasks(tasks []state.Task) []map[string]any {
	result := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, managementTask(task))
	}
	return result
}

func managementTask(task state.Task) map[string]any {
	raw, _ := json.Marshal(task)
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return map[string]any{"id": sanitizeText(task.ID), "name": sanitizeText(task.Name)}
	}
	if task.NextRunAt.IsZero() {
		delete(value, "next_run_at")
	}
	if task.LastRunAt == nil || task.LastRunAt.IsZero() {
		delete(value, "last_run_at")
	}
	value["id"] = sanitizeText(task.ID)
	value["name"] = sanitizeText(task.Name)
	value["prompt"] = sanitizeText(task.Prompt)
	value["model"] = sanitizeText(task.Model)
	value["last_status"] = sanitizeText(task.LastStatus)
	return value
}

func (p *pluginRuntime) saveTasks(body []byte) (managementResponse, error) {
	_, done, accepted := p.beginOperation()
	if !accepted {
		return managementJSONError(http.StatusServiceUnavailable, "plugin_unavailable", "插件正在停止或尚未就绪"), nil
	}
	defer done()
	if len(body) > 1<<20 {
		return managementJSONError(http.StatusRequestEntityTooLarge, "request_too_large", "任务数据过大"), nil
	}
	var request saveTasksRequest
	if err := json.Unmarshal(body, &request); err != nil {
		var tasks []state.Task
		if json.Unmarshal(body, &tasks) != nil {
			return managementJSONError(http.StatusBadRequest, "invalid_tasks", "任务 JSON 无效"), nil
		}
		request.Tasks = tasks
	}
	if len(request.Tasks) > 100 {
		return managementJSONError(http.StatusBadRequest, "too_many_tasks", "最多保存 100 个任务"), nil
	}
	now := time.Now().UTC()
	seen := make(map[string]struct{}, len(request.Tasks))
	normalized := make([]state.Task, 0, len(request.Tasks))
	for index, task := range request.Tasks {
		task.ID = strings.TrimSpace(task.ID)
		if task.ID == "" {
			task.ID = fmt.Sprintf("task-%d-%d", now.UnixNano(), index)
		}
		if !validSafeID(task.ID, 160) {
			return managementJSONError(http.StatusBadRequest, "invalid_task_id", "任务 ID 无效"), nil
		}
		if _, exists := seen[task.ID]; exists {
			return managementJSONError(http.StatusBadRequest, "duplicate_task", "任务 ID 重复"), nil
		}
		seen[task.ID] = struct{}{}
		task.Name = strings.TrimSpace(task.Name)
		if task.Name == "" {
			task.Name = "Codex 唤醒任务"
		}
		if len(task.Name) > 100 || len(task.Prompt) > 2000 || len(task.Model) > 160 {
			return managementJSONError(http.StatusBadRequest, "task_field_too_long", "任务字段过长"), nil
		}
		if !validSafeID(task.Name, 100) || strings.ContainsFunc(task.Prompt, unicode.IsControl) || strings.ContainsFunc(task.Model, unicode.IsControl) {
			return managementJSONError(http.StatusBadRequest, "invalid_task_text", "任务文本包含非法控制字符"), nil
		}
		task.Prompt = strings.TrimSpace(task.Prompt)
		task.Model = strings.TrimSpace(task.Model)
		// Validate the raw schedule first. Normalization is intentionally
		// forgiving for legacy state files, but must not silently turn an
		// unknown kind or an out-of-range startup delay into a valid task.
		if err := state.ValidateSchedule(task.Schedule); err != nil {
			return managementJSONError(http.StatusBadRequest, "invalid_schedule", scrubError(err)), nil
		}
		task.Schedule = state.NormalizeSchedule(task.Schedule, config.DefaultTaskInterval.String())
		if task.Schedule.Kind == state.ScheduleKindInterval {
			interval, _ := config.ParseTaskInterval(task.Schedule.Interval)
			task.Schedule.Interval = interval.String()
		}
		if task.MaxOutputTokens < 0 || task.MaxOutputTokens > 4096 {
			return managementJSONError(http.StatusBadRequest, "invalid_output_tokens", "max_output_tokens 必须为 0 到 4096"), nil
		}
		task.AccountIDs = uniqueNonEmpty(task.AccountIDs)
		for _, accountID := range task.AccountIDs {
			if !validSafeID(accountID, 256) {
				return managementJSONError(http.StatusBadRequest, "invalid_account_id", "账号必须使用有效的 auth_index"), nil
			}
		}
		if task.CreatedAt.IsZero() {
			task.CreatedAt = now
		}
		// NextRunAt is recomputed below once old runtime fields and quota cache
		// are known. Client-supplied runtime timestamps are never trusted.
		task.NextRunAt = time.Time{}
		normalized = append(normalized, task)
	}
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	p.mu.Lock()
	previous := make(map[string]state.Task, len(p.state.Tasks))
	for _, oldTask := range p.state.Tasks {
		previous[oldTask.ID] = oldTask
	}
	for index := range normalized {
		task := &normalized[index]
		if oldTask, ok := previous[task.ID]; ok {
			// Runtime-owned fields cannot be forged by the management client.
			task.CreatedAt = oldTask.CreatedAt
			task.LastRunAt = oldTask.LastRunAt
			task.LastStatus = oldTask.LastStatus
			task.SuccessCount = oldTask.SuccessCount
			task.FailureCount = oldTask.FailureCount
			task.QuotaHandledResets = oldTask.QuotaHandledResets
			task.QuotaRetries = oldTask.QuotaRetries
			if schedulesEquivalent(oldTask.Schedule, task.Schedule) {
				task.NextRunAt = oldTask.NextRunAt
			}
		} else {
			// CreatedAt is runtime-owned. A client must not backdate a new task
			// to force an immediate interval/quota execution.
			task.CreatedAt = now
			task.LastRunAt = nil
			task.LastStatus = ""
			task.SuccessCount = 0
			task.FailureCount = 0
			task.QuotaHandledResets = nil
			task.QuotaRetries = nil
		}
		if task.NextRunAt.IsZero() {
			if state.NormalizeSchedule(task.Schedule, state.DefaultInterval).Kind == state.ScheduleKindQuotaReset {
				task.NextRunAt = p.quotaNextRunAtLocked(*task, now.In(time.Local))
			} else {
				task.NextRunAt = state.NextRunAt(*task, now.In(time.Local), nil)
			}
		}
	}
	p.state.Tasks = normalized
	p.state.Normalize(now, config.DefaultTaskInterval.String(), p.cfg.HistoryLimit)
	snapshot, path, historyLimit := p.state.Clone(), p.statePath, p.cfg.HistoryLimit
	p.mu.Unlock()
	if err := p.persistSnapshot(path, snapshot, historyLimit); err != nil {
		return managementJSONError(http.StatusInternalServerError, "state_save_failed", scrubError(err)), nil
	}
	return p.jsonManagementResponse(map[string]any{"ok": true, "tasks": managementTasks(normalized)})
}

type wakeRequest struct {
	TaskID          string   `json:"task_id,omitempty"`
	AccountIDs      []string `json:"account_ids,omitempty"`
	AuthIndices     []string `json:"auth_indices,omitempty"`
	Prompt          string   `json:"prompt,omitempty"`
	Model           string   `json:"model,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
}

func (p *pluginRuntime) wake(body []byte) (managementResponse, error) {
	var request wakeRequest
	if len(body) > 64<<10 || json.Unmarshal(body, &request) != nil {
		return managementJSONError(http.StatusBadRequest, "invalid_wake_request", "唤醒请求 JSON 无效"), nil
	}
	if len(request.Prompt) > 2000 || len(request.Model) > 160 || request.MaxOutputTokens < 0 || request.MaxOutputTokens > 4096 {
		return managementJSONError(http.StatusBadRequest, "invalid_wake_request", "唤醒参数无效"), nil
	}
	operationContext, done, accepted := p.beginOperation()
	if !accepted {
		return managementJSONError(http.StatusServiceUnavailable, "plugin_unavailable", "插件正在停止或尚未就绪"), nil
	}
	defer done()
	selection := uniqueNonEmpty(append(append([]string{}, request.AccountIDs...), request.AuthIndices...))
	p.mu.RLock()
	bridgeAvailable := p.host != nil && !p.closed
	var task state.Task
	if request.TaskID != "" {
		for _, candidate := range p.state.Tasks {
			if candidate.ID == strings.TrimSpace(request.TaskID) {
				task = candidate
				break
			}
		}
	}
	p.mu.RUnlock()
	if !bridgeAvailable {
		return managementJSONError(http.StatusServiceUnavailable, "host_unavailable", "宿主回调不可用"), nil
	}
	if request.TaskID != "" && task.ID == "" {
		return managementJSONError(http.StatusNotFound, "task_not_found", "任务不存在"), nil
	}
	if task.ID == "" {
		task = state.Task{ID: "", Name: "手动唤醒", Enabled: true, AccountIDs: selection, CreatedAt: time.Now().UTC(), Schedule: state.Schedule{Interval: config.DefaultTaskInterval.String()}}
	}
	override := &wakeSettings{Model: strings.TrimSpace(request.Model), Prompt: strings.TrimSpace(request.Prompt), MaxOutputTokens: request.MaxOutputTokens}
	// For a named task, an omitted account selection means the task's own
	// account_ids. An explicit selection (including one account) overrides it;
	// an ad-hoc wake with no selection intentionally means all eligible accounts.
	var selected []string
	if len(selection) > 0 || task.ID == "" {
		selected = selection
	}
	record, err := p.executeTask(operationContext, task, "manual", selected, override, task.ID != "")
	if err != nil {
		return managementJSONError(http.StatusInternalServerError, "wake_failed", scrubError(err)), nil
	}
	return p.jsonManagementResponse(map[string]any{"ok": true, "run": sanitizeRunRecord(record)})
}

func sanitizeRunRecord(record state.RunRecord) state.RunRecord {
	record.RunID = sanitizeText(record.RunID)
	record.TaskID = sanitizeText(record.TaskID)
	record.Trigger = sanitizeText(record.Trigger)
	record.Status = sanitizeText(record.Status)
	record.Results = append([]state.AccountResult(nil), record.Results...)
	for index := range record.Results {
		result := &record.Results[index]
		result.AuthIndex = sanitizeText(result.AuthIndex)
		result.Label = sanitizeText(result.Label)
		result.Status = sanitizeText(result.Status)
		result.Error = sanitizeText(result.Error)
	}
	return record
}

func (p *pluginRuntime) historyResponse(query map[string][]string) (managementResponse, error) {
	stateValue, _, _, _ := p.stateSnapshot()
	limit := 50
	if values := query["limit"]; len(values) > 0 {
		if parsed, err := strconv.Atoi(values[0]); err == nil && parsed > 0 && parsed <= 300 {
			limit = parsed
		}
	}
	if len(stateValue.History) > limit {
		stateValue.History = stateValue.History[len(stateValue.History)-limit:]
	}
	history := make([]state.RunRecord, len(stateValue.History))
	for index, record := range stateValue.History {
		history[index] = sanitizeRunRecord(record)
	}
	return p.jsonManagementResponse(map[string]any{"history": history})
}

func (p *pluginRuntime) diagnostics() map[string]any {
	stateValue, cfg, path, running := p.stateSnapshot()
	p.mu.RLock()
	scheduler := p.scheduler
	hasHost := p.host != nil
	p.mu.RUnlock()
	reason := "running"
	switch {
	case !cfg.Enabled:
		reason = "plugin_disabled"
	case !cfg.AutoWake:
		reason = "auto_wake_disabled"
	case !hasHost:
		reason = "host_unavailable"
	case !running:
		reason = "worker_stopped"
	}
	now := time.Now()
	return map[string]any{
		"plugin": pluginName, "version": pluginVersion, "schema_version": schemaVersion, "abi_version": abiVersion,
		"enabled": cfg.Enabled, "auto_wake": cfg.AutoWake, "worker_running": running, "scheduler_status": reason,
		"state_file": relativeStatePath(path), "tasks": len(stateValue.Tasks), "history": len(stateValue.History),
		"sensitive_logging": false, "upstream_url": cfg.UpstreamURL, "last_error": scheduler.LastError,
		"scan_interval": cfg.ScanInterval.String(), "request_timeout": cfg.RequestTimeout.String(),
		"server_local_time": now.Format("2006-01-02 15:04:05 -07:00"), "server_timezone": now.Format("MST (UTC-07:00)"),
		"scheduler": scheduler,
	}
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (p *pluginRuntime) statusResource() managementResponse {
	return managementResponse{StatusCode: http.StatusOK, Headers: map[string][]string{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "Content-Security-Policy": {"default-src 'none'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'"}}, Body: []byte(codexWakeupHTMLV2)}
}
