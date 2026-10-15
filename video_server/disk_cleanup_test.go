package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		valid bool
	}{
		{input: "1024", want: 1024, valid: true},
		{input: "500MB", want: 500_000_000, valid: true},
		{input: "1.5GiB", want: 1_610_612_736, valid: true},
		{input: "0", want: 0, valid: true},
		{input: "-1GB", valid: false},
		{input: "nonsense", valid: false},
		{input: "999999999999999999999T", valid: false},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseByteSize(test.input)
			if (err == nil) != test.valid {
				t.Fatalf("parseByteSize(%q) error = %v; valid %v", test.input, err, test.valid)
			}
			if err == nil && got != test.want {
				t.Errorf("parseByteSize(%q) = %d, want %d", test.input, got, test.want)
			}
		})
	}
}

func TestCleanupContinuousRecordingsOldestFirstAndProtectsActive(t *testing.T) {
	dir := t.TempDir()
	files := map[string]int64{
		"old!2000!1000.webm":     5,
		"newer!3000!1000.mp4":    10,
		"RoutineID_12_abcd.webm": 20,
		"bad!timestamp!10.webm":  30,
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := cleanupContinuousRecordings(dir, 5, func(string) (int64, error) {
		var free int64
		for name, size := range files {
			if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
				free += size
			}
		}
		return free, nil
	}, func(name string) bool {
		return name == "newer!3000!1000.mp4"
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d files, want 1", deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "old!2000!1000.webm")); !os.IsNotExist(err) {
		t.Errorf("oldest continuous recording should have been removed; stat error = %v", err)
	}
	for _, name := range []string{"newer!3000!1000.mp4", "RoutineID_12_abcd.webm", "bad!timestamp!10.webm"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should have been preserved: %v", name, err)
		}
	}
}

func TestContinuousRecordingStartTime(t *testing.T) {
	got, ok := continuousRecordingStartTime("camera!1700!300.webm")
	if !ok || got != 1400 {
		t.Fatalf("continuousRecordingStartTime() = (%d, %v), want (1400, true)", got, ok)
	}
	if _, ok := continuousRecordingStartTime("RoutineID_2_abcd.webm"); ok {
		t.Fatal("triggered recording should not be classified as continuous")
	}
}

func TestCleanupPreservesLastHourUnlessAtHalfMinimum(t *testing.T) {
	tests := []struct {
		name          string
		initialFree   int64
		wantRecentDel bool
	}{
		{name: "preserve recent above half minimum", initialFree: 80, wantRecentDel: false},
		{name: "allow recent at half minimum", initialFree: 50, wantRecentDel: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.UnixMilli(1_700_000_000_000)
			oldStart := now.Add(-2 * time.Hour).UnixMilli()
			recentStart := now.Add(-30 * time.Minute).UnixMilli()
			oldName := "camera!" + strconv.FormatInt(oldStart, 10) + "!0.webm"
			recentName := "camera!" + strconv.FormatInt(recentStart, 10) + "!0.webm"
			for _, name := range []string{oldName, recentName} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("video"), 0600); err != nil {
					t.Fatal(err)
				}
			}

			deleted, err := cleanupContinuousRecordingsAt(dir, 100, func(string) (int64, error) {
				free := test.initialFree
				for _, name := range []string{oldName, recentName} {
					if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
						free += 10
					}
				}
				return free, nil
			}, nil, now)
			if err != nil {
				t.Fatal(err)
			}
			wantDeleted := 1
			if test.wantRecentDel {
				wantDeleted = 2
			}
			if deleted != wantDeleted {
				t.Errorf("deleted %d recordings, want %d", deleted, wantDeleted)
			}
			_, recentErr := os.Stat(filepath.Join(dir, recentName))
			if gotDeleted := os.IsNotExist(recentErr); gotDeleted != test.wantRecentDel {
				t.Errorf("recent recording deleted = %v, want %v", gotDeleted, test.wantRecentDel)
			}
		})
	}
}
