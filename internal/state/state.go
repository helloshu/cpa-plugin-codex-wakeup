package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const CurrentVersion = 2

const (
	ScheduleKindDaily      = "daily"
	ScheduleKindWeekly     = "weekly"
	ScheduleKindInterval   = "interval"
	ScheduleKindQuotaReset = "quota_reset"
	ScheduleKindStartup    = "startup"
	QuotaWindowEither      = "either"
	QuotaWindowPrimary     = "primary_window"
	QuotaWindowSecondary   = "secondary_window"
	DefaultDailyTime       = "09:00"
	DefaultWeeklyTime      = "10:00"
	DefaultInterval        = "5h"
	MinimumInterval        = time.Minute
	MaximumInterval        = 30 * 24 * time.Hour
	MaximumStartupDelay    = 7 * 24 * time.Hour
	QuotaResetDelay        = time.Minute
)

var ErrCorruptState = errors.New("corrupt codex-wakeup state was quarantined")

type Schedule struct {
	// Kind is empty in the original v0.1 state format. Empty values are
	// normalized to interval so {"interval":"5h"} remains usable.
	Kind                string `json:"kind,omitempty"`
	Interval            string `json:"interval,omitempty"`
	DailyTime           string `json:"daily_time,omitempty"`
	WeeklyDays          []int  `json:"weekly_days,omitempty"`
	WeeklyTime          string `json:"weekly_time,omitempty"`
	QuotaResetWindow    string `json:"quota_reset_window,omitempty"`
	StartupDelayMinutes int    `json:"startup_delay_minutes,omitempty"`
}

type Task struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Enabled    bool     `json:"enabled"`
	AccountIDs []string `json:"account_ids,omitempty"`
	Prompt     string   `json:"prompt,omitempty"`
	Model      string   `json:"model,omitempty"`
	// MaxOutputTokens is retained for state/config compatibility. The
	// ChatGPT Codex backend rejects this outbound field, so the plugin does
	// not send it and cannot promise a hard output-token limit.
	MaxOutputTokens int        `json:"max_output_tokens,omitempty"`
	Schedule        Schedule   `json:"schedule"`
	CreatedAt       time.Time  `json:"created_at"`
	LastRunAt       *time.Time `json:"last_run_at,omitempty"`
	LastStatus      string     `json:"last_status,omitempty"`
	NextRunAt       time.Time  `json:"next_run_at"`
	SuccessCount    int64      `json:"success_count"`
	FailureCount    int64      `json:"failure_count"`
	// Per-account reset boundary already attempted by this quota task. A nil
	// map denotes legacy state, which used the task-wide LastRunAt baseline.
	QuotaHandledResets map[string]time.Time  `json:"quota_handled_resets,omitempty"`
	QuotaRetries       map[string]QuotaRetry `json:"quota_retries,omitempty"`
}

type QuotaRetry struct {
	ResetAt     time.Time `json:"reset_at"`
	NextRetryAt time.Time `json:"next_retry_at"`
	Failures    int       `json:"failures"`
}

type AccountState struct {
	AuthIndex      string    `json:"auth_index"`
	Label          string    `json:"label,omitempty"`
	MaskedEmail    string    `json:"email,omitempty"`
	LastRunAt      time.Time `json:"last_run_at,omitempty"`
	LastStatus     string    `json:"last_status,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	LastHTTPStatus int       `json:"last_http_status,omitempty"`
	LastDurationMS int64     `json:"last_duration_ms,omitempty"`
	InputTokens    int64     `json:"input_tokens,omitempty"`
	OutputTokens   int64     `json:"output_tokens,omitempty"`
	TotalTokens    int64     `json:"total_tokens,omitempty"`
	SuccessCount   int64     `json:"success_count"`
	FailureCount   int64     `json:"failure_count"`
	// Quota fields contain only timestamps and sanitized diagnostics. OAuth
	// material and the usage response body are never persisted.
	PrimaryResetAt   *time.Time `json:"primary_reset_at,omitempty"`
	SecondaryResetAt *time.Time `json:"secondary_reset_at,omitempty"`
	// Keep the most recent elapsed boundary when usage rolls forward. Tasks
	// consume it independently using LastRunAt (or CreatedAt), including after
	// a restart between the refresh and execution.
	PrimaryElapsedResetAt   *time.Time `json:"primary_elapsed_reset_at,omitempty"`
	SecondaryElapsedResetAt *time.Time `json:"secondary_elapsed_reset_at,omitempty"`
	QuotaLastRefreshAt      *time.Time `json:"quota_last_refresh_at,omitempty"`
	QuotaNextRefreshAt      *time.Time `json:"quota_next_refresh_at,omitempty"`
	QuotaLastError          string     `json:"quota_last_error,omitempty"`
	QuotaRefreshFailures    int        `json:"quota_refresh_failures,omitempty"`
}

type AccountResult struct {
	AuthIndex    string `json:"auth_index"`
	Label        string `json:"label,omitempty"`
	Status       string `json:"status"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	TotalTokens  int64  `json:"total_tokens,omitempty"`
	Error        string `json:"error,omitempty"`
}

type RunRecord struct {
	RunID       string          `json:"run_id"`
	TaskID      string          `json:"task_id,omitempty"`
	Trigger     string          `json:"trigger"`
	Status      string          `json:"status"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt time.Time       `json:"completed_at"`
	Results     []AccountResult `json:"results"`
}

type State struct {
	Version  int                     `json:"version"`
	Tasks    []Task                  `json:"tasks"`
	Accounts map[string]AccountState `json:"accounts"`
	History  []RunRecord             `json:"history"`
}

func Empty() State {
	return State{Version: CurrentVersion, Tasks: []Task{}, Accounts: map[string]AccountState{}, History: []RunRecord{}}
}

func (s *State) Normalize(now time.Time, defaultInterval string, historyLimit int) {
	if s == nil {
		return
	}
	if s.Version < CurrentVersion {
		s.Version = CurrentVersion
	}
	if s.Accounts == nil {
		s.Accounts = make(map[string]AccountState)
	}
	if s.Tasks == nil {
		s.Tasks = []Task{}
	}
	if s.History == nil {
		s.History = []RunRecord{}
	}
	seenTasks := make(map[string]struct{}, len(s.Tasks))
	normalizedTasks := make([]Task, 0, len(s.Tasks))
	for _, task := range s.Tasks {
		task.ID = strings.TrimSpace(task.ID)
		if task.ID == "" {
			continue
		}
		if _, exists := seenTasks[task.ID]; exists {
			continue
		}
		seenTasks[task.ID] = struct{}{}
		task.AccountIDs = uniqueStrings(task.AccountIDs)
		task.Schedule = NormalizeSchedule(task.Schedule, defaultInterval)
		if task.CreatedAt.IsZero() {
			task.CreatedAt = now
		}
		if task.NextRunAt.IsZero() {
			task.NextRunAt = NextRunAt(task, now, nil)
		}
		normalizedTasks = append(normalizedTasks, task)
	}
	s.Tasks = normalizedTasks
	if historyLimit > 0 && len(s.History) > historyLimit {
		s.History = append([]RunRecord(nil), s.History[len(s.History)-historyLimit:]...)
	}
}

func (s State) Clone() State {
	clone := Empty()
	clone.Version = s.Version
	clone.Tasks = append([]Task(nil), s.Tasks...)
	for index := range clone.Tasks {
		clone.Tasks[index].AccountIDs = append([]string(nil), clone.Tasks[index].AccountIDs...)
		clone.Tasks[index].Schedule.WeeklyDays = append([]int(nil), clone.Tasks[index].Schedule.WeeklyDays...)
		if clone.Tasks[index].QuotaHandledResets != nil {
			clone.Tasks[index].QuotaHandledResets = make(map[string]time.Time, len(s.Tasks[index].QuotaHandledResets))
			for key, value := range s.Tasks[index].QuotaHandledResets {
				clone.Tasks[index].QuotaHandledResets[key] = value
			}
		}
		if clone.Tasks[index].QuotaRetries != nil {
			clone.Tasks[index].QuotaRetries = make(map[string]QuotaRetry, len(s.Tasks[index].QuotaRetries))
			for key, value := range s.Tasks[index].QuotaRetries {
				clone.Tasks[index].QuotaRetries[key] = value
			}
		}
	}
	clone.Accounts = make(map[string]AccountState, len(s.Accounts))
	for key, value := range s.Accounts {
		clone.Accounts[key] = value
	}
	clone.History = make([]RunRecord, len(s.History))
	for index, item := range s.History {
		clone.History[index] = item
		clone.History[index].Results = append([]AccountResult(nil), item.Results...)
	}
	return clone
}

func AppendHistory(s *State, item RunRecord, historyLimit int) {
	if s == nil {
		return
	}
	s.History = append(s.History, item)
	if historyLimit > 0 && len(s.History) > historyLimit {
		s.History = append([]RunRecord(nil), s.History[len(s.History)-historyLimit:]...)
	}
}

// Interval returns a task's validated interval, falling back when an older
// state file contains an empty or malformed value.
func Interval(task Task, fallback string) time.Duration {
	return parseIntervalOrDefault(task.Schedule.Interval, fallback)
}

func Load(path string) (State, error) {
	empty := Empty()
	path = strings.TrimSpace(path)
	if path == "" {
		return empty, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("read state: %w", err)
	}
	var loaded State
	if err = json.Unmarshal(raw, &loaded); err != nil {
		backup := quarantine(path)
		if backup == "" {
			return empty, fmt.Errorf("%w: decode failed: %v", ErrCorruptState, err)
		}
		return empty, fmt.Errorf("%w: decode failed; backup=%s", ErrCorruptState, backup)
	}
	if loaded.Version > CurrentVersion {
		return empty, fmt.Errorf("state version %d is newer than supported version %d", loaded.Version, CurrentVersion)
	}
	loaded.Normalize(time.Now(), DefaultInterval, 300)
	return loaded, nil
}

func Save(path string, value State, historyLimit int) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("state path is empty")
	}
	value.Normalize(time.Now(), DefaultInterval, historyLimit)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".codex-wakeup-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod state temp file: %w", err)
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(value); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync state temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close state temp file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func quarantine(path string) string {
	for index := 0; index < 100; index++ {
		suffix := fmt.Sprintf(".corrupt-%s", time.Now().UTC().Format("20060102T150405.000000000Z"))
		if index > 0 {
			suffix += fmt.Sprintf("-%d", index)
		}
		backup := path + suffix
		if _, err := os.Stat(backup); err == nil {
			continue
		}
		if err := os.Rename(path, backup); err == nil {
			return backup
		}
	}
	return ""
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func parseIntervalOrDefault(raw, fallback string) time.Duration {
	if duration, err := time.ParseDuration(strings.TrimSpace(raw)); err == nil && duration > 0 {
		return duration
	}
	if duration, err := time.ParseDuration(strings.TrimSpace(fallback)); err == nil && duration > 0 {
		return duration
	}
	return 5 * time.Hour
}
