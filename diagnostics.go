package main

import (
	"time"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/state"
)

// Process-local heartbeat: unlike execution history this also shows scans
// which have not finished (for example a blocking host HTTP callback).
type schedulerDiagnostics struct {
	StartedAt           *time.Time `json:"started_at,omitempty"`
	LastScanAt          *time.Time `json:"last_scan_at,omitempty"`
	LastScanCompletedAt *time.Time `json:"last_scan_completed_at,omitempty"`
	ScanCount           uint64     `json:"scan_count"`
	Phase               string     `json:"phase"`
	CurrentTaskID       string     `json:"current_task_id,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

func (p *pluginRuntime) setScanPhase(phase, taskID string) {
	p.mu.Lock()
	p.scheduler.Phase = phase
	p.scheduler.CurrentTaskID = sanitizeText(taskID)
	p.mu.Unlock()
}

func (p *pluginRuntime) logTaskNotDue(task state.Task) {
	reason := "not_due"
	if !task.Enabled {
		reason = "task_disabled"
	} else if task.Schedule.Kind == state.ScheduleKindQuotaReset {
		reason = "no_eligible_elapsed_quota_reset"
	}
	p.log("debug", "scheduler task skipped", map[string]any{
		"task_id": sanitizeText(task.ID), "reason": reason, "next_run_at": task.NextRunAt,
	})
}

func (p *pluginRuntime) logRunResult(record state.RunRecord, err error) {
	clean := sanitizeRunRecord(record)
	fields := map[string]any{
		"run_id": clean.RunID, "task_id": clean.TaskID, "trigger": clean.Trigger,
		"status": clean.Status, "duration_ms": time.Since(record.StartedAt).Milliseconds(),
		"results": clean.Results,
	}
	level := "info"
	if err != nil {
		level = "warn"
		fields["error"] = scrubError(err)
	} else if record.Status == "failed" || record.Status == "partial" {
		level = "warn"
		for _, result := range clean.Results {
			if result.Error != "" {
				fields["error"] = result.Error
				break
			}
		}
	}
	p.log(level, "task run finished", fields)
}
