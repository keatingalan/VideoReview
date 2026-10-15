package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	diskCleanupEnabled   = true
	minimumFreeDiskSpace = int64(10 * 1024 * 1024 * 1024)

	diskCleanupMu sync.Mutex
)

type recordingCandidate struct {
	name      string
	path      string
	startTime int64
}

func parseByteSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("size cannot be empty")
	}

	multipliers := map[string]float64{
		"B": 1,
		"K": 1e3, "KB": 1e3,
		"M": 1e6, "MB": 1e6,
		"G": 1e9, "GB": 1e9,
		"T": 1e12, "TB": 1e12,
		"KIB": 1 << 10,
		"MIB": 1 << 20,
		"GIB": 1 << 30,
		"TIB": 1 << 40,
	}

	upper := strings.ToUpper(value)
	suffix := "B"
	number := upper
	for _, candidate := range []string{"KIB", "MIB", "GIB", "TIB", "KB", "MB", "GB", "TB", "K", "M", "G", "T", "B"} {
		if strings.HasSuffix(upper, candidate) {
			suffix = candidate
			number = strings.TrimSpace(strings.TrimSuffix(upper, candidate))
			break
		}
	}

	amount, err := strconv.ParseFloat(number, 64)
	if err != nil || amount < 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, fmt.Errorf("expected a non-negative byte size, got %q", value)
	}
	bytes := amount * multipliers[suffix]
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("size is too large")
	}
	return int64(math.Ceil(bytes)), nil
}

func continuousRecordingStartTime(name string) (int64, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".webm" && ext != ".mp4" {
		return 0, false
	}
	parts := strings.SplitN(name, "!", 3)
	if len(parts) != 3 || parts[0] == "" {
		return 0, false
	}
	endTime, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	duration, err := strconv.ParseInt(strings.TrimSuffix(parts[2], filepath.Ext(name)), 10, 64)
	if err != nil || duration < 0 {
		return 0, false
	}
	return endTime - duration, true
}

// runDiskCleanup serializes cleanup with active-upload registration so no file
// currently being written can be selected for deletion.
func runDiskCleanup() {
	if !diskCleanupEnabled || minimumFreeDiskSpace <= 0 {
		return
	}

	diskCleanupMu.Lock()
	defer diskCleanupMu.Unlock()

	var deleted int
	var err error
	withActiveUploadsLocked(func(isActive func(string) bool) {
		deleted, err = cleanupContinuousRecordings(uploadDir, minimumFreeDiskSpace, diskFreeBytes, isActive)
	})
	if err != nil {
		log.Printf("Disk cleanup failed: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("Disk cleanup removed %d old continuous recording(s)", deleted)
	}
}

func cleanupContinuousRecordings(dir string, minimumFree int64, freeSpace func(string) (int64, error), isProtected func(string) bool) (int, error) {
	return cleanupContinuousRecordingsAt(dir, minimumFree, freeSpace, isProtected, time.Now())
}

func cleanupContinuousRecordingsAt(dir string, minimumFree int64, freeSpace func(string) (int64, error), isProtected func(string) bool, now time.Time) (int, error) {
	free, err := freeSpace(dir)
	if err != nil {
		return 0, fmt.Errorf("check free space: %w", err)
	}
	if free >= minimumFree {
		return 0, nil
	}

	entries, err := os.ReadDir(dir)
	// Preserve the most recent hour during ordinary cleanup. At or below half
	// the configured minimum, reclaiming recent footage is allowed as a last resort.
	allowRecent := free <= minimumFree/2
	recentCutoff := now.Add(-time.Hour).UnixMilli()
	if err != nil {
		return 0, fmt.Errorf("list recordings: %w", err)
	}
	candidates := make([]recordingCandidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		startTime, ok := continuousRecordingStartTime(entry.Name())
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		candidates = append(candidates, recordingCandidate{
			name:      entry.Name(),
			path:      filepath.Join(dir, entry.Name()),
			startTime: startTime,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].startTime == candidates[j].startTime {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].startTime < candidates[j].startTime
	})

	deleted := 0
	for _, candidate := range candidates {
		if free >= minimumFree {
			break
		}
		if isProtected != nil && isProtected(candidate.name) {
			continue
		}
		if !allowRecent && candidate.startTime >= recentCutoff {
			continue
		}
		if err := os.Remove(candidate.path); err != nil {
			// A locked file should not prevent reclaiming space from later files.
			continue
		}
		deleted++
		free, err = freeSpace(dir)
		if err != nil {
			return deleted, fmt.Errorf("check free space after deleting %q: %w", candidate.name, err)
		}
	}
	return deleted, nil
}
