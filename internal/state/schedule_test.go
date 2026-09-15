package state

import (
	"testing"
	"time"
)

func TestLegacyIntervalScheduleNormalizes(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	last := now.Add(-6 * time.Hour)
	task := Task{ID: "legacy", Enabled: true, CreatedAt: now.Add(-10 * time.Hour), LastRunAt: &last, Schedule: Schedule{Interval: "5h"}}
	task.NextRunAt = NextRunAt(task, now, nil)
	if got := NormalizeSchedule(task.Schedule, DefaultInterval).Kind; got != ScheduleKindInterval {
		t.Fatalf("legacy kind = %q", got)
	}
	if !task.NextRunAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("legacy next = %s", task.NextRunAt)
	}
}

func TestDailyNextRunCrossesLocalDay(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, zone)
	task := Task{Enabled: true, CreatedAt: now, Schedule: Schedule{Kind: ScheduleKindDaily, DailyTime: "09:00"}}
	want := time.Date(2026, 9, 6, 9, 0, 0, 0, zone).UTC()
	if got := NextRunAt(task, now, nil); !got.Equal(want) {
		t.Fatalf("daily next = %s, want %s", got, want)
	}
}

func TestWeeklyNextRunCrossesLocalWeek(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, zone) // Saturday
	task := Task{Enabled: true, CreatedAt: now, Schedule: Schedule{Kind: ScheduleKindWeekly, WeeklyDays: []int{1}, WeeklyTime: "08:30"}}
	want := time.Date(2026, 9, 7, 8, 30, 0, 0, zone).UTC()
	if got := NextRunAt(task, now, nil); !got.Equal(want) {
		t.Fatalf("weekly next = %s, want %s", got, want)
	}
}

func TestScheduleValidation(t *testing.T) {
	tests := []struct {
		name string
		s    Schedule
		ok   bool
	}{
		{"daily", Schedule{Kind: ScheduleKindDaily, DailyTime: "23:59"}, true},
		{"weekly", Schedule{Kind: ScheduleKindWeekly, WeeklyDays: []int{0, 6}, WeeklyTime: "00:00"}, true},
		{"interval", Schedule{Kind: ScheduleKindInterval, Interval: "5h"}, true},
		{"quota", Schedule{Kind: ScheduleKindQuotaReset, QuotaResetWindow: QuotaWindowEither}, true},
		{"startup", Schedule{Kind: ScheduleKindStartup, StartupDelayMinutes: 30}, true},
		{"bad time", Schedule{Kind: ScheduleKindDaily, DailyTime: "24:00"}, false},
		{"no weekly day", Schedule{Kind: ScheduleKindWeekly, WeeklyTime: "10:00"}, false},
		{"too short", Schedule{Kind: ScheduleKindInterval, Interval: "10s"}, false},
		{"bad quota", Schedule{Kind: ScheduleKindQuotaReset, QuotaResetWindow: "other"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidateSchedule(test.s) == nil; got != test.ok {
				t.Fatalf("ValidateSchedule() = %t, want %t", got, test.ok)
			}
		})
	}
}

func TestQuotaDueUsesOnlyPassedResetAfterLastRun(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	last := now.Add(-2 * time.Hour)
	task := Task{Enabled: true, LastRunAt: &last, Schedule: Schedule{Kind: ScheduleKindQuotaReset, QuotaResetWindow: QuotaWindowEither}}
	if !DueQuota(task, now, []time.Time{now.Add(-time.Hour)}) {
		t.Fatal("passed reset should be due")
	}
	if DueQuota(task, now, []time.Time{now.Add(time.Hour)}) {
		t.Fatal("future reset should not be due")
	}
	if DueQuota(task, now, []time.Time{last}) {
		t.Fatal("reset at last_run_at should not be due")
	}
}

func TestQuotaDueUsesCreatedAtWhenNeverRun(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour)
	task := Task{Enabled: true, CreatedAt: created, Schedule: Schedule{Kind: ScheduleKindQuotaReset, QuotaResetWindow: QuotaWindowEither}}
	if DueQuota(task, now, []time.Time{created.Add(-time.Minute)}) {
		t.Fatal("reset before task creation should not be due")
	}
	if !DueQuota(task, now, []time.Time{created.Add(time.Minute)}) {
		t.Fatal("reset after task creation should be due")
	}
}

func TestQuotaPreviewAndDueIncludeOneMinuteDelay(t *testing.T) {
	reset := time.Date(2026, 9, 15, 6, 11, 55, 0, time.UTC)
	task := Task{Enabled: true, CreatedAt: reset.Add(-time.Hour), Schedule: Schedule{Kind: ScheduleKindQuotaReset}}
	triggerAt := reset.Add(time.Minute)
	for _, now := range []time.Time{reset.Add(-time.Second), reset, triggerAt.Add(-time.Nanosecond)} {
		if DueQuota(task, now, []time.Time{reset}) {
			t.Fatalf("quota task became due at %s before grace period elapsed", now)
		}
		if next := NextRunAt(task, now, []time.Time{reset}); !next.Equal(triggerAt) {
			t.Fatalf("preview at %s = %s, want %s", now, next, triggerAt)
		}
	}
	if !DueQuota(task, triggerAt, []time.Time{reset}) {
		t.Fatal("quota task was not due exactly one minute after reset")
	}
}

func TestScheduleValidationRejectsUnknownKindAndClampedStartupDelay(t *testing.T) {
	if err := ValidateSchedule(Schedule{Kind: "bogus", Interval: "5h"}); err == nil {
		t.Fatal("unknown schedule kind unexpectedly accepted")
	}
	if err := ValidateSchedule(Schedule{Kind: ScheduleKindStartup, StartupDelayMinutes: int(MaximumStartupDelay/time.Minute) + 1}); err == nil {
		t.Fatal("out-of-range startup delay unexpectedly accepted")
	}
}
