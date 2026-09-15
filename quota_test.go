package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/state"
)

type quotaRuntimeHost struct {
	*runtimeFakeHost
	quotaResponses map[string]host.HTTPResponse
	quotaRequests  int
}

type lateQuotaHost struct {
	*quotaRuntimeHost
	delay time.Duration
}

type changingQuotaListHost struct {
	*runtimeFakeHost
	onList  func()
	listErr error
}

func (h *changingQuotaListHost) ListAuthFiles(ctx context.Context) ([]host.AuthFile, error) {
	if h.onList != nil {
		h.onList()
	}
	if h.listErr != nil {
		return nil, h.listErr
	}
	return h.runtimeFakeHost.ListAuthFiles(ctx)
}

func (h *lateQuotaHost) HTTPDo(ctx context.Context, request host.HTTPRequest) (host.HTTPResponse, error) {
	if request.Method == "GET" && request.URL == quotaUsageURL {
		time.Sleep(h.delay)
	}
	return h.quotaRuntimeHost.HTTPDo(ctx, request)
}

func (h *quotaRuntimeHost) HTTPDo(ctx context.Context, request host.HTTPRequest) (host.HTTPResponse, error) {
	if request.Method == "GET" && request.URL == quotaUsageURL {
		token := ""
		if values := request.Headers["Authorization"]; len(values) > 0 {
			token = strings.TrimPrefix(values[0], "Bearer ")
		}
		h.mu.Lock()
		h.quotaRequests++
		response, ok := h.quotaResponses[token]
		h.mu.Unlock()
		if !ok {
			return host.HTTPResponse{StatusCode: 500, Body: []byte(`{"error":{"message":"missing quota fixture"}}`)}, nil
		}
		return response, nil
	}
	return h.runtimeFakeHost.HTTPDo(ctx, request)
}

func quotaTask(id string, accounts []string) state.Task {
	now := time.Now().UTC().Add(-time.Hour)
	return state.Task{ID: id, Name: id, Enabled: true, AccountIDs: accounts, CreatedAt: now, LastRunAt: timePtr(now), Schedule: state.Schedule{Kind: state.ScheduleKindQuotaReset, QuotaResetWindow: state.QuotaWindowEither}}
}

func TestUnavailableAccountIsMonitoredAndWokenAfterReset(t *testing.T) {
	base := newRuntimeFakeHost()
	base.entries[0].Unavailable = true
	base.entries[0].Status = "unavailable"
	now := time.Now().UTC()
	quotaHost := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 200, Body: []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"reset_at":%d}}}`, now.Add(-2*time.Minute).Unix()))},
	}}
	p := newRuntimeForTest(t, quotaHost)
	p.state.Tasks = []state.Task{quotaTask("unavailable", []string{"auth-a"})}
	p.runDue(context.Background(), false)
	if quotaHost.quotaRequests != 1 || fmt.Sprint(requestTokens(base)) != "[token-a]" {
		t.Fatalf("unavailable account was dropped: queries=%d wakes=%v", quotaHost.quotaRequests, requestTokens(base))
	}
	if !base.entries[0].Unavailable {
		t.Fatal("plugin must not rewrite host availability")
	}
	p.runDue(context.Background(), false)
	if len(requestTokens(base)) != 1 {
		t.Fatal("same reset was repeated")
	}
}

func TestDisabledOrDeletedAccountDoesNotRunFromCachedQuota(t *testing.T) {
	for _, mode := range []string{"disabled", "status_disabled", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			base := newRuntimeFakeHost()
			switch mode {
			case "disabled":
				base.entries[0].Disabled = true
			case "status_disabled":
				base.entries[0].Status = "disabled"
			case "deleted":
				base.entries = base.entries[1:]
			}
			p := newRuntimeForTest(t, base)
			now := time.Now().UTC()
			p.state.Tasks = []state.Task{quotaTask("paused", []string{"auth-a"})}
			p.state.Accounts["auth-a"] = state.AccountState{AuthIndex: "auth-a", PrimaryResetAt: timePtr(now.Add(-2 * time.Minute)), QuotaNextRefreshAt: timePtr(now.Add(time.Hour))}
			p.runDue(context.Background(), false)
			if len(requestTokens(base)) != 0 || len(p.state.History) != 0 || len(p.state.Tasks[0].QuotaHandledResets) != 0 {
				t.Fatal("cached reset caused a run or consumed progress for an absent/disabled account")
			}
		})
	}
}

func TestQuotaWakeRetriesPersistAndDoNotRepeatSuccessfulAccounts(t *testing.T) {
	for _, code := range []int{0, 401, 403, 408, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			base := newRuntimeFakeHost()
			base.entries[0].Unavailable = true
			if code == 0 {
				base.errors["token-a"] = errors.New("network timeout")
			} else {
				base.responses["token-a"] = host.HTTPResponse{StatusCode: code, Body: []byte(`{"error":{"message":"temporarily rejected"}}`)}
			}
			p := newRuntimeForTest(t, base)
			now := time.Now().UTC()
			reset := now.Add(-2 * time.Minute)
			p.state.Tasks = []state.Task{quotaTask("retry", []string{"auth-a", "auth-b"})}
			for _, id := range []string{"auth-a", "auth-b"} {
				p.state.Accounts[id] = state.AccountState{AuthIndex: id, PrimaryResetAt: timePtr(reset), QuotaNextRefreshAt: timePtr(now.Add(time.Hour))}
			}
			p.runDue(context.Background(), false)
			task := p.state.Tasks[0]
			retry, ok := task.QuotaRetries["auth-a"]
			if !ok || !retry.ResetAt.Equal(reset) || retry.Failures != 1 || !task.QuotaHandledResets["auth-b"].Equal(reset) || !task.QuotaHandledResets["auth-a"].Before(reset) {
				t.Fatalf("failed A and successful B did not retain independent progress: %#v", task)
			}
			delay := 2 * time.Minute
			if code == 401 || code == 403 {
				delay = 30 * time.Minute
			}
			if !retry.NextRetryAt.Equal(task.LastRunAt.Add(delay)) || !task.NextRunAt.Equal(retry.NextRetryAt) {
				t.Fatalf("retry delay/preview mismatch: %#v", task)
			}
			p.runDue(context.Background(), false)
			if len(requestTokens(base)) != 2 {
				t.Fatal("retry ignored the backoff")
			}
			loaded, err := state.Load(p.statePath)
			if err != nil {
				t.Fatal(err)
			}
			restarted := newRuntimeForTest(t, base)
			restarted.state = loaded
			restarted.runDue(context.Background(), false)
			if len(requestTokens(base)) != 2 {
				t.Fatal("restart lost the retry deadline")
			}
			if got := restarted.quotaDueAccounts(loaded.Tasks[0], retry.NextRetryAt); fmt.Sprint(got) != "[auth-a]" {
				t.Fatalf("retry became due for wrong accounts: %v", got)
			}
			// Simulate the deadline elapsing and host credentials being renewed.
			pending := loaded.Tasks[0].QuotaRetries["auth-a"]
			pending.NextRetryAt = time.Now().UTC().Add(-time.Second)
			restarted.state.Tasks[0].QuotaRetries["auth-a"] = pending
			base.materials["auth-a"] = []byte(`{"access_token":"renewed-token","account_id":"acct-a"}`)
			base.responses["renewed-token"] = base.responses["token-b"]
			base.entries[0].Disabled = true
			restarted.runDue(context.Background(), false)
			if len(requestTokens(base)) != 2 {
				t.Fatal("pending retry bypassed explicit disable")
			}
			base.entries[0].Disabled = false
			restarted.runDue(context.Background(), false)
			restarted.runDue(context.Background(), false)
			if got := requestTokens(base); len(got) != 3 || !strings.Contains(fmt.Sprint(got), "renewed-token") {
				t.Fatalf("retry did not use current credentials or repeated B: %v", got)
			}
			if len(restarted.state.Tasks[0].QuotaRetries) != 0 || !restarted.state.Tasks[0].QuotaHandledResets["auth-a"].Equal(reset) {
				t.Fatal("successful retry did not clear the pending reset")
			}
		})
	}
}

func TestQuotaQueryRecoveryResumesUnavailableAccountAfterRestart(t *testing.T) {
	base := newRuntimeFakeHost()
	base.entries[0].Unavailable = true
	fixture := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 401, Body: []byte(`{"error":{"message":"expired"}}`)},
	}}
	p := newRuntimeForTest(t, fixture)
	now := time.Now().UTC()
	p.state.Tasks = []state.Task{quotaTask("query-recovery", []string{"auth-a"})}
	p.state.Accounts["auth-a"] = state.AccountState{AuthIndex: "auth-a", PrimaryResetAt: timePtr(now.Add(-2 * time.Minute))}
	p.runDue(context.Background(), false)
	if fixture.quotaRequests != 1 || len(requestTokens(base)) != 0 || p.state.Accounts["auth-a"].QuotaLastError == "" {
		t.Fatal("failed query must be visible without using stale reset to wake")
	}
	loaded, err := state.Load(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newRuntimeForTest(t, fixture)
	restarted.state = loaded
	restarted.runDue(context.Background(), false)
	if fixture.quotaRequests != 1 {
		t.Fatal("restart ignored quota query backoff")
	}
	account := restarted.state.Accounts["auth-a"]
	account.QuotaNextRefreshAt = timePtr(now.Add(-time.Second))
	restarted.state.Accounts["auth-a"] = account
	fixture.quotaResponses["token-a"] = host.HTTPResponse{StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"reset_after_seconds":18000}}}`)}
	restarted.runDue(context.Background(), false)
	if fixture.quotaRequests != 2 || len(requestTokens(base)) != 1 || restarted.state.Accounts["auth-a"].QuotaLastError != "" {
		t.Fatal("successful query did not resume unavailable account's pending reset")
	}
}

func TestQuotaRetryBackoffCapsAndPermanentRequestErrorDoesNotLoop(t *testing.T) {
	if quotaRetryDelay(1) != 2*time.Minute || quotaRetryDelay(2) != 4*time.Minute || quotaRetryDelay(100) != 30*time.Minute {
		t.Fatal("unexpected retry backoff")
	}
	base := newRuntimeFakeHost()
	base.responses["token-a"] = host.HTTPResponse{StatusCode: 400, Body: []byte(`{"error":{"message":"invalid model"}}`)}
	p := newRuntimeForTest(t, base)
	now := time.Now().UTC()
	p.state.Tasks = []state.Task{quotaTask("bad-request", []string{"auth-a"})}
	p.state.Accounts["auth-a"] = state.AccountState{AuthIndex: "auth-a", PrimaryResetAt: timePtr(now.Add(-2 * time.Minute)), QuotaNextRefreshAt: timePtr(now.Add(time.Hour))}
	p.runDue(context.Background(), false)
	p.runDue(context.Background(), false)
	if len(requestTokens(base)) != 1 || len(p.state.Tasks[0].QuotaRetries) != 0 || p.state.Tasks[0].LastStatus != "failed" {
		t.Fatal("permanent request error should be reported without a retry loop")
	}
}

func TestQuotaProbeRechecksTaskAndHostBeforeExecution(t *testing.T) {
	for _, scenario := range []string{"list_failed", "task_disabled", "task_deleted", "selection_changed"} {
		t.Run(scenario, func(t *testing.T) {
			base := newRuntimeFakeHost()
			fixture := &changingQuotaListHost{runtimeFakeHost: base}
			p := newRuntimeForTest(t, fixture)
			now := time.Now().UTC()
			p.state.Tasks = []state.Task{quotaTask("recheck", []string{"auth-a"})}
			p.state.Accounts["auth-a"] = state.AccountState{AuthIndex: "auth-a", PrimaryResetAt: timePtr(now.Add(-2 * time.Minute)), QuotaNextRefreshAt: timePtr(now.Add(time.Hour))}
			if scenario == "list_failed" {
				fixture.listErr = errors.New("host listing failed")
			} else {
				calls := 0
				fixture.onList = func() {
					calls++
					if calls != 2 {
						return
					}
					p.mu.Lock()
					defer p.mu.Unlock()
					switch scenario {
					case "task_disabled":
						p.state.Tasks[0].Enabled = false
					case "task_deleted":
						p.state.Tasks = nil
					case "selection_changed":
						p.state.Tasks[0].AccountIDs = []string{"auth-b"}
					}
				}
			}
			p.runDue(context.Background(), false)
			if len(requestTokens(base)) != 0 {
				t.Fatal("stale host/task state bypassed the quota probe gate")
			}
		})
	}
}

func TestParseQuotaUsageSupportsResetAtAndResetAfterSeconds(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	snapshot, err := parseQuotaUsage([]byte(`{"rate_limit":{"primary_window":{"reset_at":1799236800},"secondary_window":{"reset_after_seconds":7200}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PrimaryResetAt == nil || snapshot.PrimaryResetAt.Unix() != 1799236800 {
		t.Fatalf("primary reset = %#v", snapshot.PrimaryResetAt)
	}
	if snapshot.SecondaryResetAt == nil || snapshot.SecondaryResetAt.Unix() != now.Add(2*time.Hour).Unix() {
		t.Fatalf("secondary reset = %#v", snapshot.SecondaryResetAt)
	}
	if _, err := parseQuotaUsage([]byte(`{"rate_limit":{"allowed":true}}`), now); err == nil {
		t.Fatal("missing windows unexpectedly succeeded")
	}
	// An absurd duration must be ignored rather than wrapping into a timestamp
	// in the past when converted to time.Duration.
	snapshot, err = parseQuotaUsage([]byte(`{"rate_limit":{"primary_window":{"reset_after_seconds":9223372036854775807}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PrimaryResetAt != nil {
		t.Fatalf("overflowing reset_after_seconds was accepted: %s", snapshot.PrimaryResetAt)
	}
}

func TestQuotaRefreshIsThrottledAndPersistsSanitizedState(t *testing.T) {
	base := newRuntimeFakeHost()
	hostFixture := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"reset_at":1799236800},"secondary_window":{"reset_after_seconds":7200}}}`)},
	}}
	p := newRuntimeForTest(t, hostFixture)
	task := quotaTask("quota", []string{"auth-a"})
	p.state.Tasks = []state.Task{task}
	p.refreshQuotaForTasks(context.Background(), []state.Task{task})
	p.refreshQuotaForTasks(context.Background(), []state.Task{task})
	hostFixture.mu.Lock()
	requests := hostFixture.quotaRequests
	hostFixture.mu.Unlock()
	if requests != 1 {
		t.Fatalf("quota requests = %d, want one within 2m", requests)
	}
	p.mu.RLock()
	account := p.state.Accounts["auth-a"]
	p.mu.RUnlock()
	if account.PrimaryResetAt == nil || account.QuotaLastRefreshAt == nil || account.QuotaNextRefreshAt == nil || account.QuotaLastError != "" {
		t.Fatalf("quota state = %#v", account)
	}
	loaded, err := state.Load(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Accounts["auth-a"].PrimaryResetAt == nil || strings.Contains(fmt.Sprint(loaded), "token-a") {
		t.Fatalf("persisted quota state leaked material: %#v", loaded)
	}
}

func TestQuotaResetTriggersOnceAndFailureDoesNotWake(t *testing.T) {
	base := newRuntimeFakeHost()
	pastReset := time.Now().UTC().Add(-time.Minute).Unix()
	hostFixture := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 200, Body: []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"reset_at":%d}}}`, pastReset))},
	}}
	p := newRuntimeForTest(t, hostFixture)
	task := quotaTask("quota-once", []string{"auth-a"})
	p.state.Tasks = []state.Task{task}
	p.runDue(context.Background(), false)
	firstTokens := requestTokens(base)
	p.runDue(context.Background(), false)
	secondTokens := requestTokens(base)
	if len(firstTokens) != 1 || len(secondTokens) != 1 {
		t.Fatalf("quota trigger count = %v then %v", firstTokens, secondTokens)
	}
	p.mu.RLock()
	if p.state.Tasks[0].LastStatus != "success" || p.state.Tasks[0].LastRunAt == nil {
		p.mu.RUnlock()
		t.Fatalf("quota task did not complete: %#v", p.state.Tasks[0])
	}
	p.mu.RUnlock()

	failingBase := newRuntimeFakeHost()
	failing := &quotaRuntimeHost{runtimeFakeHost: failingBase, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 503, Body: []byte(`{"error":{"message":"busy"}}`)},
	}}
	p2 := newRuntimeForTest(t, failing)
	task2 := quotaTask("quota-fail", []string{"auth-a"})
	p2.state.Tasks = []state.Task{task2}
	p2.runDue(context.Background(), false)
	if got := requestTokens(failingBase); len(got) != 0 {
		t.Fatalf("failed quota refresh unexpectedly woke model: %v", got)
	}
	p2.mu.RLock()
	account := p2.state.Accounts["auth-a"]
	p2.mu.RUnlock()
	if account.QuotaLastError == "" || account.QuotaNextRefreshAt == nil {
		t.Fatalf("failed quota refresh state = %#v", account)
	}
}

func TestQuotaRefreshDeduplicatesDuplicateHostEntries(t *testing.T) {
	base := newRuntimeFakeHost()
	base.entries = append(base.entries, base.entries[0])
	hostFixture := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"reset_after_seconds":7200}}}`)},
	}}
	p := newRuntimeForTest(t, hostFixture)
	task := quotaTask("quota-dedupe", []string{"auth-a"})
	p.refreshQuotaForTasks(context.Background(), []state.Task{task})
	hostFixture.mu.Lock()
	requests := hostFixture.quotaRequests
	hostFixture.mu.Unlock()
	if requests != 1 {
		t.Fatalf("duplicate host entries caused %d quota requests", requests)
	}
}

func TestQuotaFailureDoesNotUseStaleResetTimestamp(t *testing.T) {
	base := newRuntimeFakeHost()
	pastReset := time.Now().UTC().Add(-time.Minute)
	baseQuota := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 503, Body: []byte(`{"error":{"message":"busy"}}`)},
	}}
	p := newRuntimeForTest(t, baseQuota)
	task := quotaTask("quota-stale", []string{"auth-a"})
	p.state.Tasks = []state.Task{task}
	p.state.Accounts["auth-a"] = state.AccountState{
		AuthIndex:          "auth-a",
		PrimaryResetAt:     timePtr(pastReset),
		QuotaNextRefreshAt: timePtr(time.Now().UTC().Add(-time.Minute)),
	}
	p.runDue(context.Background(), false)
	if got := requestTokens(base); len(got) != 0 {
		t.Fatalf("stale reset unexpectedly woke model: %v", got)
	}
}

func TestQuotaRefreshRejectsLateHostResponse(t *testing.T) {
	base := newRuntimeFakeHost()
	quotaHost := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"reset_after_seconds":7200}}}`)},
	}}
	hostFixture := &lateQuotaHost{quotaRuntimeHost: quotaHost, delay: 20 * time.Millisecond}
	p := newRuntimeForTest(t, hostFixture)
	p.cfg.RequestTimeout = time.Millisecond
	task := quotaTask("quota-timeout", []string{"auth-a"})
	p.state.Tasks = []state.Task{task}
	p.refreshQuotaForTasks(context.Background(), []state.Task{task})
	p.mu.RLock()
	account := p.state.Accounts["auth-a"]
	p.mu.RUnlock()
	if account.PrimaryResetAt != nil || !strings.Contains(strings.ToLower(account.QuotaLastError), "deadline") {
		t.Fatalf("late quota response was accepted: %#v", account)
	}
}

func TestQuotaRolloverDoesNotLoseDueReset(t *testing.T) {
	for _, window := range []string{state.QuotaWindowPrimary, state.QuotaWindowSecondary, state.QuotaWindowEither} {
		for _, cleared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cleared=%t", window, cleared), func(t *testing.T) {
				base := newRuntimeFakeHost()
				now := time.Now().UTC()
				past := now.Add(-time.Minute)
				future := now.Add(5 * time.Hour)
				field := state.QuotaWindowPrimary
				if window == state.QuotaWindowSecondary {
					field = state.QuotaWindowSecondary
				}
				body := fmt.Sprintf(`{"rate_limit":{"%s":{"reset_at":%d}}}`, field, future.Unix())
				if cleared {
					body = fmt.Sprintf(`{"rate_limit":{"%s":{}}}`, field)
				}
				quotaHost := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
					"token-a": {StatusCode: 200, Body: []byte(body)},
				}}
				p := newRuntimeForTest(t, quotaHost)
				// Both tasks must see the same reset, even after the first task
				// has completed. Consuming an account-wide flag would lose one.
				for _, id := range []string{"first", "second"} {
					task := quotaTask(id, []string{"auth-a"})
					task.Schedule.QuotaResetWindow = window
					p.state.Tasks = append(p.state.Tasks, task)
				}
				account := state.AccountState{AuthIndex: "auth-a", QuotaNextRefreshAt: timePtr(past)}
				if field == state.QuotaWindowPrimary {
					account.PrimaryResetAt = timePtr(past)
				} else {
					account.SecondaryResetAt = timePtr(past)
				}
				p.state.Accounts["auth-a"] = account
				p.runDue(context.Background(), false)
				if got := requestTokens(base); len(got) != 2 {
					t.Fatalf("rollover lost a due reset: requests = %v, want two", got)
				}
				loaded, err := state.Load(p.statePath)
				if err != nil {
					t.Fatal(err)
				}
				for _, task := range loaded.Tasks {
					if !cleared && task.NextRunAt.Unix() != future.Add(time.Minute).Unix() {
						t.Fatalf("next trigger preview = %s, want reset + one minute", task.NextRunAt)
					}
				}
				restarted := newRuntimeForTest(t, quotaHost)
				restarted.state = loaded
				restarted.runDue(context.Background(), false)
				if got := requestTokens(base); len(got) != 2 {
					t.Fatalf("restart repeated a consumed reset: %v", got)
				}
			})
		}
	}
}

func TestQuotaAccountsRunIndependentlyAfterOneMinute(t *testing.T) {
	base := newRuntimeFakeHost()
	p := newRuntimeForTest(t, base)
	now := time.Now().UTC()
	aReset, bReset := now.Add(-2*time.Minute), now.Add(-30*time.Second)
	task := quotaTask("independent", []string{"auth-a", "auth-b"})
	p.state.Tasks = []state.Task{task}
	p.state.Accounts["auth-a"] = state.AccountState{AuthIndex: "auth-a", PrimaryResetAt: timePtr(aReset), QuotaNextRefreshAt: timePtr(now.Add(time.Hour))}
	p.state.Accounts["auth-b"] = state.AccountState{AuthIndex: "auth-b", PrimaryResetAt: timePtr(bReset), QuotaNextRefreshAt: timePtr(now.Add(time.Hour))}
	p.runDue(context.Background(), false)
	if got := requestTokens(base); fmt.Sprint(got) != "[token-a]" {
		t.Fatalf("A reset should not wake B during B's grace period: %v", got)
	}
	saved := p.state.Tasks[0]
	if !saved.NextRunAt.Equal(bReset.Add(time.Minute)) {
		t.Fatalf("next preview = %s, want B reset + one minute", saved.NextRunAt)
	}
	if got := p.quotaDueAccounts(saved, bReset.Add(time.Minute-time.Nanosecond)); len(got) != 0 {
		t.Fatalf("B became due before the full minute: %v", got)
	}
	if got := p.quotaDueAccounts(saved, bReset.Add(time.Minute)); fmt.Sprint(got) != "[auth-b]" {
		t.Fatalf("A's completion suppressed B's later trigger: %v", got)
	}
	// Move B's cached boundary into the elapsed state for a second real scan.
	// It is still earlier than A's completion, exercising the old shared cursor.
	b := p.state.Accounts["auth-b"]
	b.PrimaryResetAt = timePtr(now.Add(-90 * time.Second))
	p.state.Accounts["auth-b"] = b
	p.runDue(context.Background(), false)
	p.runDue(context.Background(), false)
	if got := requestTokens(base); fmt.Sprint(got) != "[token-a token-b]" {
		t.Fatalf("each account should run only for its own reset: %v", got)
	}
	loaded, err := state.Load(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newRuntimeForTest(t, base)
	restarted.state = loaded
	restarted.runDue(context.Background(), false)
	if got := requestTokens(base); len(got) != 2 {
		t.Fatalf("restart lost per-account cursors: %v", got)
	}
}

func TestQuotaBusyAccountIsRetriedWithoutRepeatingOtherAccounts(t *testing.T) {
	base := newRuntimeFakeHost()
	p := newRuntimeForTest(t, base)
	now := time.Now().UTC()
	task := quotaTask("busy-account", []string{"auth-a", "auth-b"})
	p.state.Tasks = []state.Task{task}
	for _, id := range task.AccountIDs {
		p.state.Accounts[id] = state.AccountState{AuthIndex: id, PrimaryResetAt: timePtr(now.Add(-2 * time.Minute)), QuotaNextRefreshAt: timePtr(now.Add(time.Hour))}
	}
	lock := p.accountLock("auth-a")
	lock.Lock()
	p.runDue(context.Background(), false)
	lock.Unlock()
	if got := requestTokens(base); fmt.Sprint(got) != "[token-b]" {
		t.Fatalf("busy A should not block B: %v", got)
	}
	p.runDue(context.Background(), false)
	p.runDue(context.Background(), false)
	if got := requestTokens(base); fmt.Sprint(got) != "[token-a token-b]" {
		t.Fatalf("busy A should remain pending without repeating B: %v", got)
	}
}

func TestQuotaRolloverSurvivesRestartBeforeExecution(t *testing.T) {
	base := newRuntimeFakeHost()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	quotaHost := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
		"token-a": {StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"reset_after_seconds":18000}}}`)},
	}}
	p := newRuntimeForTest(t, quotaHost)
	task := quotaTask("pending", []string{"auth-a"})
	p.state.Tasks = []state.Task{task}
	p.state.Accounts["auth-a"] = state.AccountState{AuthIndex: "auth-a", PrimaryResetAt: timePtr(past)}
	p.refreshQuotaForTasks(context.Background(), p.state.Tasks)
	loaded, err := state.Load(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newRuntimeForTest(t, quotaHost)
	restarted.state = loaded
	restarted.runDue(context.Background(), false)
	restarted.runDue(context.Background(), false)
	if got := requestTokens(base); len(got) != 1 {
		t.Fatalf("pending reset should survive restart and run once: %v", got)
	}
}

func TestQuotaRolloverRequiresSuccessfulRefreshAndEligibleBoundary(t *testing.T) {
	for _, scenario := range []string{"recovered", "future", "before_creation", "other_window"} {
		t.Run(scenario, func(t *testing.T) {
			base := newRuntimeFakeHost()
			quotaHost := &quotaRuntimeHost{runtimeFakeHost: base, quotaResponses: map[string]host.HTTPResponse{
				"token-a": {StatusCode: 503, Body: []byte(`{"error":{"message":"busy"}}`)},
			}}
			p := newRuntimeForTest(t, quotaHost)
			task := quotaTask("quota", []string{"auth-a"})
			now := time.Now().UTC()
			previous := now.Add(-time.Minute)
			switch scenario {
			case "future":
				previous = now.Add(time.Hour)
			case "before_creation":
				task.CreatedAt = now
				task.LastRunAt = nil
			case "other_window":
				task.Schedule.QuotaResetWindow = state.QuotaWindowSecondary
			}
			p.state.Tasks = []state.Task{task}
			p.state.Accounts["auth-a"] = state.AccountState{AuthIndex: "auth-a", PrimaryResetAt: timePtr(previous)}
			p.runDue(context.Background(), false)
			if got := requestTokens(base); len(got) != 0 {
				t.Fatalf("failed refresh woke a task: %v", got)
			}
			account := p.state.Accounts["auth-a"]
			account.QuotaNextRefreshAt = timePtr(now.Add(-time.Minute))
			p.state.Accounts["auth-a"] = account
			quotaHost.quotaResponses["token-a"] = host.HTTPResponse{StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"reset_after_seconds":18000}}}`)}
			p.runDue(context.Background(), false)
			want := 0
			if scenario == "recovered" {
				want = 1
			}
			if got := requestTokens(base); len(got) != want {
				t.Fatalf("requests after successful refresh = %v, want %d", got, want)
			}
		})
	}
}
