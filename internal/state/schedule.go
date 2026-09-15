package state

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var ErrInvalidSchedule = errors.New("invalid codex-wakeup schedule")

// NormalizeSchedule upgrades the original interval-only schedule and fills
// harmless display defaults. It intentionally does not manufacture a quota
// reset timestamp: that value is only known after an official usage refresh.
func NormalizeSchedule(raw Schedule, fallback string) Schedule {
	schedule := raw
	schedule.Kind = strings.ToLower(strings.TrimSpace(schedule.Kind))
	if schedule.Kind == "" {
		schedule.Kind = ScheduleKindInterval
	}
	if schedule.Kind != ScheduleKindDaily && schedule.Kind != ScheduleKindWeekly &&
		schedule.Kind != ScheduleKindInterval && schedule.Kind != ScheduleKindQuotaReset &&
		schedule.Kind != ScheduleKindStartup {
		schedule.Kind = ScheduleKindInterval
	}
	schedule.Interval = strings.TrimSpace(schedule.Interval)
	if schedule.Interval == "" {
		schedule.Interval = strings.TrimSpace(fallback)
		if schedule.Interval == "" {
			schedule.Interval = DefaultInterval
		}
	}
	if duration, err := time.ParseDuration(schedule.Interval); err == nil && duration > 0 {
		schedule.Interval = duration.String()
	}
	schedule.DailyTime = strings.TrimSpace(schedule.DailyTime)
	if schedule.DailyTime == "" {
		schedule.DailyTime = DefaultDailyTime
	}
	schedule.WeeklyTime = strings.TrimSpace(schedule.WeeklyTime)
	if schedule.WeeklyTime == "" {
		schedule.WeeklyTime = DefaultWeeklyTime
	}
	schedule.WeeklyDays = normalizeWeekdays(schedule.WeeklyDays)
	if len(schedule.WeeklyDays) == 0 {
		schedule.WeeklyDays = []int{1, 2, 3, 4, 5}
	}
	if schedule.QuotaResetWindow == "" {
		schedule.QuotaResetWindow = QuotaWindowEither
	}
	if schedule.StartupDelayMinutes < 0 {
		schedule.StartupDelayMinutes = 0
	}
	if schedule.StartupDelayMinutes > int(MaximumStartupDelay/time.Minute) {
		schedule.StartupDelayMinutes = int(MaximumStartupDelay / time.Minute)
	}
	return schedule
}

func normalizeWeekdays(values []int) []int {
	seen := make(map[int]struct{}, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value < 0 || value > 6 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Ints(result)
	return result
}

func parseScheduleTime(value, field string) error {
	text := strings.TrimSpace(value)
	if len(text) != len("15:04") {
		return fmt.Errorf("%w: %s must use HH:MM", ErrInvalidSchedule, field)
	}
	if _, err := time.Parse("15:04", text); err != nil {
		return fmt.Errorf("%w: %s must use HH:MM", ErrInvalidSchedule, field)
	}
	return nil
}

// ValidateSchedule performs strict server-side validation for task CRUD.
// Weekday values use the Cockpit convention Sunday=0 through Saturday=6.
func ValidateSchedule(schedule Schedule) error {
	// Validate the caller's values before normalization. NormalizeSchedule is
	// intentionally forgiving for state-file migration, but using it first for
	// CRUD would turn an unknown kind into interval and clamp out-of-range
	// startup delays instead of rejecting them.
	kind := strings.ToLower(strings.TrimSpace(schedule.Kind))
	if kind == "" {
		// The v0.1 state format stored only interval, so an omitted kind is the
		// one legacy value that remains valid.
		kind = ScheduleKindInterval
	}
	switch kind {
	case ScheduleKindDaily:
		value := strings.TrimSpace(schedule.DailyTime)
		if value == "" {
			value = DefaultDailyTime
		}
		return parseScheduleTime(value, "daily_time")
	case ScheduleKindWeekly:
		if len(schedule.WeeklyDays) == 0 {
			return fmt.Errorf("%w: weekly_days must contain at least one day", ErrInvalidSchedule)
		}
		for _, day := range schedule.WeeklyDays {
			if day < 0 || day > 6 {
				return fmt.Errorf("%w: weekly_days must be between 0 and 6", ErrInvalidSchedule)
			}
		}
		value := strings.TrimSpace(schedule.WeeklyTime)
		if value == "" {
			value = DefaultWeeklyTime
		}
		if err := parseScheduleTime(value, "weekly_time"); err != nil {
			return err
		}
	case ScheduleKindInterval:
		value := strings.TrimSpace(schedule.Interval)
		if value == "" {
			value = DefaultInterval
		}
		if _, err := parseTaskInterval(value); err != nil {
			return err
		}
	case ScheduleKindQuotaReset:
		window := strings.TrimSpace(schedule.QuotaResetWindow)
		if window == "" {
			window = QuotaWindowEither
		}
		switch window {
		case QuotaWindowEither, QuotaWindowPrimary, QuotaWindowSecondary:
		default:
			return fmt.Errorf("%w: quota_reset_window must be either, primary_window, or secondary_window", ErrInvalidSchedule)
		}
	case ScheduleKindStartup:
		if schedule.StartupDelayMinutes < 0 || time.Duration(schedule.StartupDelayMinutes)*time.Minute > MaximumStartupDelay {
			return fmt.Errorf("%w: startup_delay_minutes must be between 0 and %d", ErrInvalidSchedule, int(MaximumStartupDelay/time.Minute))
		}
	default:
		return fmt.Errorf("%w: unsupported schedule kind", ErrInvalidSchedule)
	}
	return nil
}

func parseTaskInterval(raw string) (time.Duration, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, fmt.Errorf("%w: interval is required", ErrInvalidSchedule)
	}
	duration, err := time.ParseDuration(text)
	if err != nil || duration < MinimumInterval || duration > MaximumInterval {
		return 0, fmt.Errorf("%w: interval must be between %s and %s", ErrInvalidSchedule, MinimumInterval, MaximumInterval)
	}
	return duration, nil
}

// NextRunAt returns the next deterministic schedule occurrence. now's
// location is used deliberately: runtime passes time.Local, while tests can
// pass a fixed location to verify day/week boundaries. Stored timestamps are
// returned in UTC for stable JSON persistence.
func NextRunAt(task Task, now time.Time, quotaResets []time.Time) time.Time {
	schedule := NormalizeSchedule(task.Schedule, DefaultInterval)
	if now.IsZero() {
		now = time.Now()
	}
	localNow := now
	if localNow.Location() == time.UTC {
		// The runtime uses time.Local for schedule calculations. For callers that
		// pass UTC explicitly, UTC is the only unambiguous local zone available.
		localNow = now.In(time.UTC)
	}
	switch schedule.Kind {
	case ScheduleKindDaily:
		minutes, ok := minutesFromTime(schedule.DailyTime)
		if !ok {
			return time.Time{}
		}
		for day := 0; day <= 370; day++ {
			date := localNow.AddDate(0, 0, day)
			candidate := time.Date(date.Year(), date.Month(), date.Day(), minutes/60, minutes%60, 0, 0, localNow.Location())
			if candidate.After(localNow) {
				return candidate.UTC()
			}
		}
	case ScheduleKindWeekly:
		minutes, ok := minutesFromTime(schedule.WeeklyTime)
		if !ok {
			return time.Time{}
		}
		weekdays := normalizeWeekdays(schedule.WeeklyDays)
		for day := 0; day <= 370; day++ {
			date := localNow.AddDate(0, 0, day)
			if !containsInt(weekdays, int(date.Weekday())) {
				continue
			}
			candidate := time.Date(date.Year(), date.Month(), date.Day(), minutes/60, minutes%60, 0, 0, localNow.Location())
			if candidate.After(localNow) {
				return candidate.UTC()
			}
		}
	case ScheduleKindInterval:
		interval, err := parseTaskInterval(schedule.Interval)
		if err != nil {
			interval = 5 * time.Hour
		}
		base := task.CreatedAt
		if task.LastRunAt != nil && !task.LastRunAt.IsZero() {
			base = *task.LastRunAt
		}
		if base.IsZero() {
			base = now
		}
		return base.Add(interval).UTC()
	case ScheduleKindQuotaReset:
		last := time.Time{}
		if task.LastRunAt != nil {
			last = *task.LastRunAt
		}
		if last.IsZero() {
			// A reset that happened before a task was created must not cause a
			// newly-created task to fire immediately. This also mirrors the
			// scheduler's last_run_at-or-created_at baseline.
			last = task.CreatedAt
		}
		if last.IsZero() {
			last = now
		}
		var candidate time.Time
		for _, reset := range quotaResets {
			if reset.IsZero() || !reset.After(last) {
				continue
			}
			triggerAt := reset.Add(QuotaResetDelay)
			if !triggerAt.After(now) {
				continue
			}
			if candidate.IsZero() || triggerAt.Before(candidate) {
				candidate = triggerAt
			}
		}
		return candidate.UTC()
	case ScheduleKindStartup:
		return time.Time{}
	}
	return time.Time{}
}

func minutesFromTime(value string) (int, bool) {
	if err := parseScheduleTime(value, "time"); err != nil {
		return 0, false
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, false
	}
	return parsed.Hour()*60 + parsed.Minute(), true
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// Due reports deterministic schedule due state. Quota tasks are evaluated by
// DueQuota after a successful, throttled usage refresh.
func Due(task Task, now time.Time, runOnStart bool) bool {
	if !task.Enabled {
		return false
	}
	kind := NormalizeSchedule(task.Schedule, DefaultInterval).Kind
	if kind == ScheduleKindQuotaReset {
		return false
	}
	if kind == ScheduleKindStartup {
		return runOnStart && task.LastRunAt == nil
	}
	if runOnStart && task.LastRunAt == nil {
		return true
	}
	next := task.NextRunAt
	if next.IsZero() {
		next = NextRunAt(task, now, nil)
	}
	return !next.IsZero() && !now.Before(next)
}

func DueQuota(task Task, now time.Time, quotaResets []time.Time) bool {
	if !task.Enabled || NormalizeSchedule(task.Schedule, DefaultInterval).Kind != ScheduleKindQuotaReset {
		return false
	}
	last := time.Time{}
	if task.LastRunAt != nil {
		last = *task.LastRunAt
	}
	if last.IsZero() {
		last = task.CreatedAt
	}
	if last.IsZero() {
		last = now
	}
	for _, reset := range quotaResets {
		if reset.IsZero() || reset.Add(QuotaResetDelay).After(now) || !reset.After(last) {
			continue
		}
		return true
	}
	return false
}
