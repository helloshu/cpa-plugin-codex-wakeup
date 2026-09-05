package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/state"
)

const (
	// This is the same read-only endpoint used by Cockpit Tools. It is a
	// constant on purpose: quota-reset tasks cannot be redirected to a
	// third-party endpoint.
	quotaUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	quotaRefreshInterval = 2 * time.Minute
	quotaMaximumBackoff  = 30 * time.Minute
	// Keep reset_after_seconds below time.Duration's whole-second capacity so
	// converting seconds to nanoseconds cannot wrap into the past.
	quotaMaximumResetAfterSeconds int64 = 9_223_372_036
)

type quotaSnapshot struct {
	PrimaryResetAt   *time.Time
	SecondaryResetAt *time.Time
}

type quotaWindow struct {
	Primary   *time.Time
	Secondary *time.Time
}

// parseQuotaUsage accepts the stable Cockpit Tools usage shape and its
// reset_after_seconds fallback. It does not retain the response body.
func parseQuotaUsage(body []byte, now time.Time) (quotaSnapshot, error) {
	if len(body) == 0 || len(body) > 2<<20 {
		return quotaSnapshot{}, errors.New("quota usage response is empty or too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return quotaSnapshot{}, errors.New("quota usage response was not valid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return quotaSnapshot{}, errors.New("quota usage response contained trailing data")
	}
	rateLimit := quotaObjectField(root, "rate_limit", "rateLimit")
	if rateLimit == nil {
		return quotaSnapshot{}, errors.New("quota usage response lacked rate_limit")
	}
	primary := quotaObjectField(rateLimit, "primary_window", "primaryWindow")
	secondary := quotaObjectField(rateLimit, "secondary_window", "secondaryWindow")
	if primary == nil && secondary == nil {
		return quotaSnapshot{}, errors.New("quota usage response lacked primary_window and secondary_window")
	}
	result := quotaSnapshot{}
	if primary != nil {
		if reset, ok := normalizeQuotaResetTime(primary, now); ok {
			result.PrimaryResetAt = &reset
		}
	}
	if secondary != nil {
		if reset, ok := normalizeQuotaResetTime(secondary, now); ok {
			result.SecondaryResetAt = &reset
		}
	}
	return result, nil
}

func quotaObjectField(object map[string]any, names ...string) map[string]any {
	for key, value := range object {
		for _, wanted := range names {
			if canonicalKey(key) != canonicalKey(wanted) {
				continue
			}
			if nested, ok := value.(map[string]any); ok {
				return nested
			}
		}
	}
	return nil
}

func normalizeQuotaResetTime(window map[string]any, now time.Time) (time.Time, bool) {
	if value, ok := quotaField(window, "reset_at", "resetAt"); ok {
		if timestamp, valid := quotaTimestamp(value); valid {
			return time.Unix(timestamp, 0).UTC(), true
		}
	}
	if value, ok := quotaField(window, "reset_after_seconds", "resetAfterSeconds"); ok {
		if seconds, valid := quotaNonNegativeInt(value); valid {
			if seconds > quotaMaximumResetAfterSeconds {
				return time.Time{}, false
			}
			return now.UTC().Add(time.Duration(seconds) * time.Second), true
		}
	}
	return time.Time{}, false
}

func quotaField(object map[string]any, names ...string) (any, bool) {
	for key, value := range object {
		for _, wanted := range names {
			if canonicalKey(key) == canonicalKey(wanted) {
				return value, true
			}
		}
	}
	return nil, false
}

func quotaTimestamp(value any) (int64, bool) {
	switch item := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(item.String(), 10, 64)
		if err != nil {
			floatValue, floatErr := strconv.ParseFloat(item.String(), 64)
			if floatErr != nil {
				return 0, false
			}
			return quotaFloatTimestamp(floatValue)
		}
		return normalizeQuotaUnix(parsed)
	case float64:
		return quotaFloatTimestamp(item)
	case int:
		return normalizeQuotaUnix(int64(item))
	case int64:
		return normalizeQuotaUnix(item)
	case string:
		text := strings.TrimSpace(item)
		if text == "" {
			return 0, false
		}
		if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
			return normalizeQuotaUnix(parsed)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, text); err == nil {
				return parsed.Unix(), parsed.Unix() >= 0
			}
		}
	}
	return 0, false
}

func quotaFloatTimestamp(value float64) (int64, bool) {
	if value < 0 || value > 9.22e18 {
		return 0, false
	}
	return normalizeQuotaUnix(int64(value))
}

func normalizeQuotaUnix(value int64) (int64, bool) {
	if value < 0 {
		return 0, false
	}
	if value > 1_000_000_000_000 {
		value /= 1000
	}
	return value, value > 0
}

func quotaNonNegativeInt(value any) (int64, bool) {
	switch item := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(item.String(), 10, 64)
		if err == nil {
			return parsed, parsed >= 0
		}
		floatValue, floatErr := strconv.ParseFloat(item.String(), 64)
		if floatErr != nil || floatValue < 0 || floatValue > 9.22e18 {
			return 0, false
		}
		return int64(floatValue), true
	case float64:
		if item < 0 || item > 9.22e18 {
			return 0, false
		}
		return int64(item), true
	case int:
		return int64(item), item >= 0
	case int64:
		return item, item >= 0
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(item), 10, 64)
		return parsed, err == nil && parsed >= 0
	default:
		return 0, false
	}
}

func quotaResetTimes(account state.AccountState, window string) []time.Time {
	times := make([]time.Time, 0, 2)
	if window == state.QuotaWindowEither || window == state.QuotaWindowPrimary {
		if account.PrimaryResetAt != nil && !account.PrimaryResetAt.IsZero() {
			times = append(times, account.PrimaryResetAt.UTC())
		}
	}
	if window == state.QuotaWindowEither || window == state.QuotaWindowSecondary {
		if account.SecondaryResetAt != nil && !account.SecondaryResetAt.IsZero() {
			times = append(times, account.SecondaryResetAt.UTC())
		}
	}
	return times
}

func quotaResetWindow(task state.Task) string {
	window := strings.TrimSpace(task.Schedule.QuotaResetWindow)
	if window == "" {
		return state.QuotaWindowEither
	}
	return window
}

func (p *pluginRuntime) quotaTimesForTaskLocked(task state.Task) []time.Time {
	selected := make(map[string]struct{}, len(task.AccountIDs))
	for _, id := range task.AccountIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			selected[trimmed] = struct{}{}
		}
	}
	result := make([]time.Time, 0, len(p.state.Accounts)*2)
	for authIndex, account := range p.state.Accounts {
		if len(selected) != 0 {
			if _, ok := selected[authIndex]; !ok {
				continue
			}
		}
		// A failed refresh leaves old timestamps available for diagnostics, but
		// stale values must not wake a quota-reset task. A later successful
		// refresh clears this error and makes the new timestamps eligible.
		if strings.TrimSpace(account.QuotaLastError) != "" {
			continue
		}
		result = append(result, quotaResetTimes(account, quotaResetWindow(task))...)
	}
	return result
}

func (p *pluginRuntime) quotaTaskDue(task state.Task, now time.Time) bool {
	p.mu.RLock()
	resets := p.quotaTimesForTaskLocked(task)
	p.mu.RUnlock()
	return state.DueQuota(task, now, resets)
}

func (p *pluginRuntime) refreshQuotaForTasks(ctx context.Context, tasks []state.Task) {
	quotaTasks := make([]state.Task, 0)
	selected := make(map[string]struct{})
	refreshAll := false
	for _, task := range tasks {
		if !task.Enabled || state.NormalizeSchedule(task.Schedule, state.DefaultInterval).Kind != state.ScheduleKindQuotaReset {
			continue
		}
		quotaTasks = append(quotaTasks, task)
		if len(task.AccountIDs) == 0 {
			refreshAll = true
		}
		for _, id := range task.AccountIDs {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				selected[trimmed] = struct{}{}
			}
		}
	}
	if len(quotaTasks) == 0 || ctx.Err() != nil {
		return
	}
	p.mu.RLock()
	bridge := p.host
	p.mu.RUnlock()
	if bridge == nil {
		return
	}
	entries, err := bridge.ListAuthFiles(ctx)
	if err != nil {
		p.log("warn", "quota usage account listing failed", map[string]any{"error": scrubError(err)})
		return
	}
	now := time.Now().UTC()
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if !eligibleAuth(entry) {
			continue
		}
		authIndex := strings.TrimSpace(entry.AuthIndex)
		entry.AuthIndex = authIndex
		if _, ok := seen[authIndex]; ok {
			// Be idempotent if a host version returns duplicate auth records.
			continue
		}
		seen[authIndex] = struct{}{}
		if !refreshAll {
			if _, ok := selected[authIndex]; !ok {
				continue
			}
		}
		if !p.quotaRefreshDue(authIndex, now) {
			continue
		}
		lock := p.accountLock(authIndex)
		if !lock.TryLock() {
			continue
		}
		_ = p.refreshAccountQuota(ctx, bridge, entry)
		lock.Unlock()
		if ctx.Err() != nil {
			return
		}
	}
}

func (p *pluginRuntime) quotaRefreshDue(authIndex string, now time.Time) bool {
	p.mu.RLock()
	account := p.state.Accounts[authIndex]
	p.mu.RUnlock()
	if account.QuotaNextRefreshAt == nil || account.QuotaNextRefreshAt.IsZero() {
		return true
	}
	return !now.Before(*account.QuotaNextRefreshAt)
}

func (p *pluginRuntime) refreshAccountQuota(ctx context.Context, bridge host.Client, entry host.AuthFile) error {
	attempt := time.Now().UTC()
	p.mu.RLock()
	requestTimeout := p.cfg.RequestTimeout
	p.mu.RUnlock()
	if requestTimeout <= 0 {
		requestTimeout = time.Minute
	}
	requestContext, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var snapshot quotaSnapshot
	var refreshErr string
	authFile, err := bridge.GetAuthFile(requestContext, entry.AuthIndex)
	if err != nil {
		refreshErr = scrubError(err)
	} else if contextErr := requestContext.Err(); contextErr != nil {
		refreshErr = scrubError(contextErr)
	} else {
		credential, parseErr := parseCodexCredential(authFile.RawJSON)
		if parseErr != nil {
			refreshErr = scrubError(formatAuthError(entry, parseErr))
		} else {
			headers := host.Header{
				"Authorization": {"Bearer " + credential.AccessToken},
				"Accept":        {"application/json"},
				"User-Agent":    {"codex-tui/0.146.0"},
			}
			if credential.AccountID != "" {
				headers["ChatGPT-Account-Id"] = []string{credential.AccountID}
			}
			response, requestErr := bridge.HTTPDo(requestContext, host.HTTPRequest{Method: http.MethodGet, URL: quotaUsageURL, Headers: headers})
			if requestErr != nil {
				refreshErr = sanitizeWithSecrets("quota usage transport: "+requestErr.Error(), credential.AccessToken)
			} else if contextErr := requestContext.Err(); contextErr != nil {
				// Some host.Client implementations cannot interrupt an in-flight
				// synchronous callback. Reject a late response even when it arrives
				// after that callback reports success.
				refreshErr = scrubError(contextErr)
			} else if response.StatusCode < 200 || response.StatusCode >= 300 {
				refreshErr = sanitizeWithSecrets(fmt.Sprintf("quota usage returned HTTP %d: %s", response.StatusCode, summarizeHTTPError(response.Body)), credential.AccessToken)
			} else {
				snapshot, parseErr = parseQuotaUsage(response.Body, attempt)
				if parseErr != nil {
					refreshErr = scrubError(parseErr)
				}
			}
		}
	}

	p.persistMu.Lock()
	p.mu.Lock()
	account := p.state.Accounts[entry.AuthIndex]
	account.AuthIndex = entry.AuthIndex
	account.Label = safeAccountLabel(entry)
	account.QuotaLastRefreshAt = timePtr(attempt)
	if refreshErr != "" {
		account.QuotaRefreshFailures++
		account.QuotaLastError = sanitizeText(refreshErr)
		backoff := quotaRefreshInterval
		for index := 1; index < account.QuotaRefreshFailures && backoff < quotaMaximumBackoff; index++ {
			backoff *= 2
			if backoff > quotaMaximumBackoff {
				backoff = quotaMaximumBackoff
			}
		}
		next := attempt.Add(backoff)
		account.QuotaNextRefreshAt = &next
	} else {
		account.PrimaryResetAt = snapshot.PrimaryResetAt
		account.SecondaryResetAt = snapshot.SecondaryResetAt
		account.QuotaLastError = ""
		account.QuotaRefreshFailures = 0
		next := attempt.Add(quotaRefreshInterval)
		account.QuotaNextRefreshAt = &next
	}
	p.state.Accounts[entry.AuthIndex] = account
	// A successful refresh updates the next-run preview of every quota task
	// that includes this account. Failed refreshes intentionally leave the
	// previous reset timestamps untouched and never make a task due.
	for index := range p.state.Tasks {
		task := &p.state.Tasks[index]
		if state.NormalizeSchedule(task.Schedule, state.DefaultInterval).Kind != state.ScheduleKindQuotaReset {
			continue
		}
		task.NextRunAt = state.NextRunAt(*task, attempt, p.quotaTimesForTaskLocked(*task))
	}
	snapshotState := p.state.Clone()
	path := p.statePath
	historyLimit := p.cfg.HistoryLimit
	p.mu.Unlock()
	persistErr := p.persistSnapshot(path, snapshotState, historyLimit)
	p.persistMu.Unlock()
	if persistErr != nil {
		p.log("warn", "quota state save failed", map[string]any{"error": scrubError(persistErr)})
	}
	if refreshErr != "" {
		return errors.New(refreshErr)
	}
	return persistErr
}

func timePtr(value time.Time) *time.Time {
	copy := value
	return &copy
}
