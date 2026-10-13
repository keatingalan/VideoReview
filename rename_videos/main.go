package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

var legacyFilename = regexp.MustCompile(`^RoutineID_([0-9]+)_(?:.+_)?([A-Za-z0-9]{6})$`)
var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9-]+`)

const routinesQuery = `SELECT id, name, apparatus, final_score, score1, score2
	FROM routines`

type routine struct {
	name      string
	apparatus string
	level     string
	score     float64
}

type storedMessage struct {
	Level      string  `json:"level"`
	FinalScore float64 `json:"finalScore"`
	Score1     float64 `json:"score1"`
	Score2     float64 `json:"score2"`
}

func main() {
	apply := flag.Bool("apply", false, "perform the renames (default is a dry run)")
	eventDataFlag := flag.String("eventdata", "", "EventData directory containing events.db and videos")
	dbFlag := flag.String("db", "", "event database path (default: EventData/events.db from the repository root)")
	videosFlag := flag.String("videos", "", "video directory (default: EventData/videos from the repository root)")
	flag.Parse()

	if flag.NArg() > 1 {
		log.Fatal("usage: rename_videos [-apply] [-eventdata DIR] [DIR] [-db FILE] [-videos DIR]")
	}
	eventDataDir := *eventDataFlag
	if flag.NArg() == 1 {
		if eventDataDir != "" {
			log.Fatal("specify the EventData directory either positionally or with -eventdata, not both")
		}
		eventDataDir = flag.Arg(0)
	}
	if eventDataDir == "" {
		eventDataDir = filepath.Join(findRepositoryRoot(), "EventData")
	}

	dbPath := *dbFlag
	if dbPath == "" {
		dbPath = filepath.Join(eventDataDir, "events.db")
	}
	videoDir := *videosFlag
	if videoDir == "" {
		videoDir = filepath.Join(eventDataDir, "videos")
	}

	if err := renameLegacyVideos(dbPath, videoDir, *apply); err != nil {
		log.Fatal(err)
	}
}

func findRepositoryRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "EventData", "events.db")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
		}
	}
}

func renameLegacyVideos(dbPath, videoDir string, apply bool) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open event database %q: %w", dbPath, err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("read event database %q: %w", dbPath, err)
	}

	routines, err := loadRoutines(db)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(videoDir)
	if err != nil {
		return fmt.Errorf("read video directory %q: %w", videoDir, err)
	}

	changed, skipped := 0, 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		oldName := entry.Name()
		base, ext := strings.TrimSuffix(oldName, filepath.Ext(oldName)), filepath.Ext(oldName)
		if !strings.EqualFold(ext, ".webm") && !strings.EqualFold(ext, ".mp4") {
			continue
		}
		match := legacyFilename.FindStringSubmatch(base)
		if match == nil {
			continue
		}

		id, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			log.Printf("SKIP %s: invalid routine ID", oldName)
			skipped++
			continue
		}
		info, ok := routines[id]
		if !ok {
			log.Printf("SKIP %s: routine %d is not in the database", oldName, id)
			skipped++
			continue
		}
		newName := buildFilename(id, match[2], ext, info)
		if newName == oldName {
			continue
		}
		newPath := filepath.Join(videoDir, newName)
		if _, err := os.Lstat(newPath); err == nil {
			log.Printf("SKIP %s: destination already exists (%s)", oldName, newName)
			skipped++
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check destination %q: %w", newPath, err)
		}

		if apply {
			if err := os.Rename(filepath.Join(videoDir, oldName), newPath); err != nil {
				log.Printf("ERROR %s -> %s: %v", oldName, newName, err)
				skipped++
				continue
			}
			log.Printf("RENAMED %s -> %s", oldName, newName)
		} else {
			log.Printf("DRY RUN %s -> %s", oldName, newName)
		}
		changed++
	}

	if apply {
		log.Printf("Finished: renamed %d file(s), skipped %d", changed, skipped)
	} else {
		log.Printf("Dry run: %d file(s) would be renamed, %d skipped; pass -apply to rename", changed, skipped)
	}
	return nil
}

func loadRoutines(db *sql.DB) (map[int64]routine, error) {
	rows, err := db.Query(routinesQuery)
	if err != nil {
		return nil, fmt.Errorf("query routines: %w", err)
	}
	defer rows.Close()

	routines := make(map[int64]routine)
	for rows.Next() {
		var id int64
		var name, apparatus sql.NullString
		var finalScore, score1, score2 sql.NullFloat64
		if err := rows.Scan(&id, &name, &apparatus, &finalScore, &score1, &score2); err != nil {
			return nil, fmt.Errorf("read routine row: %w", err)
		}
		info := routine{}
		if name.Valid {
			info.name = name.String
		}
		if apparatus.Valid {
			info.apparatus = apparatus.String
		}
		if finalScore.Valid && finalScore.Float64 != 0 {
			info.score = finalScore.Float64
		} else if score1.Valid && score1.Float64 != 0 {
			info.score = score1.Float64
		} else if score2.Valid && score2.Float64 != 0 {
			info.score = score2.Float64
		}
		routines[id] = info
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate routines: %w", err)
	}

	messages, err := db.Query(`SELECT routine_id, message FROM messages WHERE routine_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query messages for level metadata: %w", err)
	}
	defer messages.Close()
	for messages.Next() {
		var id int64
		var raw string
		if err := messages.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("read message row: %w", err)
		}
		info, ok := routines[id]
		if !ok {
			continue
		}
		var msg storedMessage
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			continue
		}
		if msg.Level != "" {
			info.level = msg.Level
		}
		if info.score == 0 {
			switch {
			case msg.FinalScore != 0:
				info.score = msg.FinalScore
			case msg.Score1 != 0:
				info.score = msg.Score1
			case msg.Score2 != 0:
				info.score = msg.Score2
			}
		}
		routines[id] = info
	}
	if err := messages.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return routines, nil
}

func buildFilename(id int64, randomSuffix, ext string, info routine) string {
	parts := []string{fmt.Sprintf("RoutineID_%d", id)}
	if name := filenamePart(info.name); name != "" {
		parts = append(parts, name)
	}
	if apparatus := filenamePart(info.apparatus); apparatus != "" {
		parts = append(parts, apparatus)
	}
	levelValue := strings.TrimSpace(info.level)
	if len(levelValue) >= len("level ") && strings.EqualFold(levelValue[:len("level ")], "level ") {
		levelValue = strings.TrimSpace(levelValue[len("level "):])
	}
	if level := filenamePart(levelValue); level != "" {
		parts = append(parts, "Level", level)
	}
	if score := formatScore(info.score); score != "" {
		parts = append(parts, "Score", score)
	}
	parts = append(parts, randomSuffix)
	return strings.Join(parts, "_") + ext
}

func filenamePart(value string) string {
	return strings.Trim(unsafeFilenameChars.ReplaceAllString(strings.TrimSpace(value), "_"), "_")
}

func formatScore(score float64) string {
	if score == 0 || math.IsNaN(score) || math.IsInf(score, 0) {
		return ""
	}
	return strconv.FormatFloat(score, 'f', -1, 64)
}
