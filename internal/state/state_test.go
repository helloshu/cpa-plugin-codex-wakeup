package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeDueAndDeduplicatesTasksAndAccounts(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	last := now.Add(-6 * time.Hour)
	s := State{Tasks: []Task{
		{ID: "task-1", AccountIDs: []string{"a", "a", "b"}, LastRunAt: &last, Schedule: Schedule{Interval: "5h"}},
		{ID: "task-1", AccountIDs: []string{"ignored"}},
	}}
	s.Normalize(now, "5h", 300)
	if len(s.Tasks) != 1 || len(s.Tasks[0].AccountIDs) != 2 {
		t.Fatalf("normalized tasks = %#v", s.Tasks)
	}
	if got := s.Tasks[0].NextRunAt; !got.Equal(last.Add(5 * time.Hour)) {
		t.Fatalf("next run = %s", got)
	}
}

func TestSaveLoadAndHistoryLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")
	s := Empty()
	for index := 0; index < 4; index++ {
		AppendHistory(&s, RunRecord{RunID: string(rune('a' + index))}, 2)
	}
	if err := Save(path, s, 2); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.History) != 2 || loaded.History[0].RunID != "c" {
		t.Fatalf("history = %#v", loaded.History)
	}
}

func TestCorruptStateIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if !errors.Is(err, ErrCorruptState) {
		t.Fatalf("Load() error = %v, want ErrCorruptState", err)
	}
	if len(loaded.Tasks) != 0 {
		t.Fatalf("loaded tasks = %#v", loaded.Tasks)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if len(entry.Name()) > len("state.json.corrupt-") && entry.Name()[:len("state.json.corrupt-")] == "state.json.corrupt-" {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt state backup was not created")
	}
}
