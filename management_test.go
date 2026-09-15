package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/state"
)

func TestSaveTasksPreservesServerRuntimeFields(t *testing.T) {
	fake := newRuntimeFakeHost()
	p := newRuntimeForTest(t, fake)
	last := time.Now().UTC().Add(-time.Hour).Round(0)
	old := testTask("same", []string{"auth-a"})
	old.CreatedAt = last.Add(-10 * time.Hour)
	old.LastRunAt = &last
	old.LastStatus = "success"
	old.NextRunAt = last.Add(4 * time.Hour)
	old.SuccessCount = 9
	old.FailureCount = 2
	old.QuotaHandledResets = map[string]time.Time{"auth-a": last}
	old.QuotaRetries = map[string]state.QuotaRetry{"auth-a": {ResetAt: last, NextRetryAt: last.Add(time.Hour), Failures: 3}}
	p.state.Tasks = []state.Task{old}
	forged := old
	forged.Name = "edited"
	forged.CreatedAt = time.Time{}
	forged.LastRunAt = nil
	forged.LastStatus = "forged"
	forged.NextRunAt = time.Now().UTC().Add(-100 * time.Hour)
	forged.SuccessCount = 999
	forged.FailureCount = 999
	forged.QuotaHandledResets = map[string]time.Time{"auth-a": last.Add(24 * time.Hour)}
	forged.QuotaRetries = map[string]state.QuotaRetry{"auth-a": {ResetAt: last, NextRetryAt: last, Failures: 999}}
	raw, _ := json.Marshal(saveTasksRequest{Tasks: []state.Task{forged, {ID: "new", Name: "new", Enabled: true, Schedule: state.Schedule{Interval: "5h"}}}})
	response, err := p.saveTasks(raw)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("saveTasks = %#v, %v", response, err)
	}
	var saved state.Task
	for _, task := range p.state.Tasks {
		if task.ID == "same" {
			saved = task
		}
	}
	if saved.Name != "edited" || !saved.CreatedAt.Equal(old.CreatedAt) || saved.LastStatus != old.LastStatus || saved.SuccessCount != old.SuccessCount || saved.FailureCount != old.FailureCount || !saved.NextRunAt.Equal(old.NextRunAt) {
		t.Fatalf("server fields were not preserved: %#v", saved)
	}
	if !saved.QuotaHandledResets["auth-a"].Equal(last) {
		t.Fatalf("client forged quota cursor: %#v", saved.QuotaHandledResets)
	}
	if saved.QuotaRetries["auth-a"] != old.QuotaRetries["auth-a"] {
		t.Fatal("client forged quota retry state")
	}
	for _, task := range p.state.Tasks {
		if task.ID == "new" {
			if task.LastRunAt != nil || task.LastStatus != "" || task.SuccessCount != 0 || task.FailureCount != 0 || task.NextRunAt.IsZero() {
				t.Fatalf("new task runtime fields = %#v", task)
			}
		}
	}
	if strings.Contains(string(response.Body), "0001-01-01") {
		t.Fatalf("response exposed zero runtime timestamp: %s", response.Body)
	}
}

func TestAccountsResponseDoesNotRenderZeroTimestamp(t *testing.T) {
	fake := newRuntimeFakeHost()
	p := newRuntimeForTest(t, fake)
	response, err := p.accountsResponse()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(response.Body), "0001-01-01") {
		t.Fatalf("accounts response exposed zero timestamp: %s", response.Body)
	}
	if strings.Contains(string(response.Body), `"id"`) {
		t.Fatalf("accounts response exposed host ID instead of only auth_index: %s", response.Body)
	}
}

func TestAccountsResponseKeepsUnavailableMonitoredAccounts(t *testing.T) {
	fake := newRuntimeFakeHost()
	fake.entries[0].Unavailable = true
	fake.entries[1].Disabled = true
	p := newRuntimeForTest(t, fake)
	p.cfg.AutoWake = true
	p.state.Tasks = []state.Task{quotaTask("monitor", []string{"auth-a"})}
	response, err := p.accountsResponse()
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Accounts []managementAccount `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Accounts) != 1 || data.Accounts[0].AuthIndex != "auth-a" || !data.Accounts[0].HostUnavailable || !data.Accounts[0].QuotaMonitoring || data.Accounts[0].WakeAvailable {
		t.Fatalf("unavailable monitoring status = %s", response.Body)
	}
}

func TestQuotaPreviewKeepsCloselySpacedAccountTriggers(t *testing.T) {
	p := newRuntimeForTest(t, newRuntimeFakeHost())
	first := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	second := first.Add(14 * time.Second)
	p.state.Accounts["auth-a"] = state.AccountState{AuthIndex: "auth-a", PrimaryResetAt: timePtr(first)}
	p.state.Accounts["auth-b"] = state.AccountState{AuthIndex: "auth-b", PrimaryResetAt: timePtr(second)}
	response, err := p.previewTask([]byte(`{"task":{"account_ids":["auth-a","auth-b"],"schedule":{"kind":"quota_reset","quota_reset_window":"primary_window"}}}`))
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("preview = %#v, %v", response, err)
	}
	var body struct {
		Preview []time.Time `json:"preview"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Preview) != 2 || !body.Preview[0].Equal(first.Add(time.Minute)) || !body.Preview[1].Equal(second.Add(time.Minute)) {
		t.Fatalf("preview lost an account within the delay interval: %s", response.Body)
	}
}

func TestManagementDiagnosticsExposeSanitizedHTTPFailures(t *testing.T) {
	secret := "oauth-secret-token-123"
	fake := newRuntimeFakeHost()
	p := newRuntimeForTest(t, fake)
	p.state.Accounts["auth-a"] = state.AccountState{
		AuthIndex:      "auth-a",
		LastStatus:     "failed",
		LastError:      "Codex request returned HTTP 400: Authorization: Bearer " + secret,
		LastHTTPStatus: 400,
		LastDurationMS: 123,
		FailureCount:   1,
	}
	p.state.History = []state.RunRecord{{
		RunID:       "run-1",
		Trigger:     "manual",
		Status:      "failed",
		StartedAt:   time.Now().UTC().Add(-time.Second),
		CompletedAt: time.Now().UTC(),
		Results: []state.AccountResult{{
			AuthIndex:  "auth-a",
			Label:      "A",
			Status:     "failed",
			HTTPStatus: 400,
			DurationMS: 123,
			Error:      "Authorization: Bearer " + secret,
		}},
	}}

	accounts, err := p.accountsResponse()
	if err != nil {
		t.Fatal(err)
	}
	accountBody := string(accounts.Body)
	if strings.Contains(accountBody, secret) {
		t.Fatalf("accounts response leaked token: %s", accountBody)
	}
	for _, want := range []string{`"last_error"`, `"last_http_status":400`, `"last_duration_ms":123`, `[REDACTED]`} {
		if !strings.Contains(accountBody, want) {
			t.Fatalf("accounts response missing %q: %s", want, accountBody)
		}
	}

	history, err := p.historyResponse(nil)
	if err != nil {
		t.Fatal(err)
	}
	historyBody := string(history.Body)
	if strings.Contains(historyBody, secret) {
		t.Fatalf("history response leaked token: %s", historyBody)
	}
	for _, want := range []string{`"error"`, `"http_status":400`, `"duration_ms":123`, `[REDACTED]`} {
		if !strings.Contains(historyBody, want) {
			t.Fatalf("history response missing %q: %s", want, historyBody)
		}
	}

	page := string(p.statusResource().Body)
	if strings.Contains(page, secret) {
		t.Fatalf("web UI leaked token: %s", page)
	}
	for _, want := range []string{"x.last_error", "x.last_http_status", "x.last_duration_ms", "r.http_status", "r.duration_ms", "r.error"} {
		if !strings.Contains(page, want) {
			t.Fatalf("web UI missing diagnostic field %q", want)
		}
	}
}

func TestTaskPreviewUsesServerCalculation(t *testing.T) {
	p := newRuntimeForTest(t, newRuntimeFakeHost())
	request := previewTaskRequest{Task: state.Task{
		AccountIDs: []string{"auth-a"},
		Schedule:   state.Schedule{Kind: state.ScheduleKindInterval, Interval: "5h"},
	}}
	body, _ := json.Marshal(request)
	response, err := p.previewTask(body)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("previewTask = %#v, %v", response, err)
	}
	var decoded struct {
		Preview         []string `json:"preview"`
		ServerLocalTime string   `json:"server_local_time"`
		ServerTimezone  string   `json:"server_timezone"`
	}
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Preview) != 3 || decoded.ServerLocalTime == "" || decoded.ServerTimezone == "" {
		t.Fatalf("preview response = %#v", decoded)
	}
	var previous time.Time
	for _, raw := range decoded.Preview {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil || (!previous.IsZero() && !value.After(previous)) {
			t.Fatalf("invalid preview sequence %q after %s: %v", raw, previous, err)
		}
		previous = value
	}

	invalid, err := p.previewTask([]byte(`{"task":{"schedule":{"kind":"weekly","weekly_days":[],"weekly_time":"10:00"}}}`))
	if err != nil || invalid.StatusCode != 400 {
		t.Fatalf("invalid preview = %#v, %v", invalid, err)
	}

	found := false
	for _, route := range managementRegistration().Routes {
		if route.Method == "POST" && route.Path == managementNamespace+"/preview" {
			found = true
		}
	}
	if !found {
		t.Fatal("management registration omitted POST /preview")
	}
	page := string(p.statusResource().Body)
	for _, want := range []string{"server_local_time", "按服务器本地时区计算", `api("preview"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("web UI missing server preview marker %q", want)
		}
	}
	if strings.Contains(page, `setKind("daily");loadAll();`) {
		t.Fatal("web UI must not call management APIs before a key is entered")
	}
	for _, want := range []string{`if(!key)throw new Error("请先输入 Management Key")`, `message("请先输入 Management Key，再点击刷新")`, `var overview=await api("overview");var values=await Promise.all`} {
		if !strings.Contains(page, want) {
			t.Fatalf("web UI missing empty-key request guard %q", want)
		}
	}
}
