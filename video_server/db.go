package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

var db *sql.DB

func initDB() {
	os.MkdirAll("EventData", 0755)
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")
	db.Exec("PRAGMA busy_timeout=5000")
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS routines (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		server      TEXT,
		apparatus   TEXT,
		competitor  TEXT,
		name        TEXT,
		club        TEXT,
		time_start  INTEGER,
		time_stop   INTEGER,
		time_score  INTEGER,
		time_score2 INTEGER,
		d           REAL,
		e           REAL,
		nd          REAL,
		final_score REAL,
		score1      REAL,
		d2          REAL,
		e2          REAL,
		nd2         REAL,
		score2      REAL
	)`)
	if err != nil {
		log.Fatalf("Failed to create routines table: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS messages (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		server     TEXT,
		message    TEXT,
		routine_id INTEGER REFERENCES routines(id)
	)`)
	if err != nil {
		log.Fatalf("Failed to create messages table: %v", err)
	}
	log.Printf("Database ready at %s", dbPath)
}

func saveMessage(msg ProScoreMessage, routineID int64) {
	msgJSON, _ := json.Marshal(msg)
	var rid sql.NullInt64
	if routineID != 0 {
		rid = sql.NullInt64{Int64: routineID, Valid: true}
	}
	if _, err := db.Exec(
		`INSERT INTO messages (server, message, routine_id) VALUES (?,?,?)`,
		msg.Server, string(msgJSON), rid,
	); err != nil {
		log.Printf("saveMessage error: %v", err)
	}
}

func saveEvent(msg ProScoreMessage) {
	var routineID int64
	var existingID int64

	const tenMin = int64(10 * 60 * 1000)
	windowStart := msg.Time - tenMin

	switch msg.Status {
	case "competing":

		var existingID int64
		err := db.QueryRow(`SELECT id FROM routines
			WHERE (competitor = ? OR ? = '') AND apparatus = ? AND server = ?
			  AND (
			    (time_stop IS NULL AND time_score IS NULL)
			  )
			  AND time_start >= ?
			ORDER BY time_start DESC LIMIT 1`,
			msg.Competitor, msg.Competitor, msg.Apparatus, msg.Server,
			msg.Time-int64(1000), windowStart,
		).Scan(&existingID)
		if existingID != 0 {
			println("Not saving start - routine already running")
			println(msg.FullMessage)
			return
		} //Don't insert - likely duplicate
		result, err := db.Exec(`INSERT INTO routines
			(server, apparatus, competitor, name, club, time_start)
			VALUES (?,?,?,?,?,?)`,
			msg.Server, msg.Apparatus, msg.Competitor,
			msg.Name, msg.Club, msg.Time,
		)
		if err != nil {
			log.Println("saveEvent insert error:", err)
		} else {
			routineID, _ = result.LastInsertId()
		}

	case "stopped", "scoring":
		if msg.Competitor == "" {

			hub.broadcast(EventMsg{
				Server:    msg.Server,
				Apparatus: msg.Apparatus,
				Status:    msg.Status,
			})
			return
		} // Don't save if we don't have a competitor ID to match on; this is likely a non-score update that arrived before the "NowUp" with competitor info.

		err := db.QueryRow(`SELECT id FROM routines
			WHERE (competitor = ? OR ? = '') AND apparatus = ? AND server = ?
			  AND (
			    (time_stop IS NULL AND time_score IS NULL)
			    OR time_stop >= ?
			  )
			  AND time_start >= ?
			ORDER BY time_start DESC LIMIT 1`,
			msg.Competitor, msg.Competitor, msg.Apparatus, msg.Server,
			msg.Time-int64(1000), windowStart,
		).Scan(&existingID)

		if err == nil {
			setClauses := []string{"time_stop = COALESCE(time_stop, ?)"}
			args := []any{msg.Time}

			hasScore := msg.FinalScore != 0 || msg.DScore != 0 || msg.EScore != 0 || msg.Score1 != 0 ||
				msg.DScore2 != 0 || msg.EScore2 != 0 || msg.Score2 != 0

			if hasScore {
				setClauses = append(setClauses, "time_score = COALESCE(time_score, ?)")
				args = append(args, msg.Time)
			}
			if msg.DScore != 0 {
				setClauses = append(setClauses, "d = COALESCE(d, ?)")
				args = append(args, msg.DScore)
			}
			if msg.EScore != 0 {
				setClauses = append(setClauses, "e = COALESCE(e, ?)")
				args = append(args, msg.EScore)
			}
			if msg.ND != 0 {
				setClauses = append(setClauses, "nd = COALESCE(nd, ?)")
				args = append(args, msg.ND)
			}
			if msg.FinalScore != 0 {
				setClauses = append(setClauses, "final_score = COALESCE(final_score, ?)")
				args = append(args, msg.FinalScore)
			}
			if msg.Score1 != 0 {
				setClauses = append(setClauses, "score1 = COALESCE(score1, ?)")
				args = append(args, msg.Score1)
			}
			if msg.DScore2 != 0 {
				setClauses = append(setClauses, "d2 = COALESCE(d2, ?)")
				args = append(args, msg.DScore2)
			}
			if msg.EScore2 != 0 {
				setClauses = append(setClauses, "e2 = COALESCE(e2, ?)")
				args = append(args, msg.EScore2)
			}
			if msg.ND2 != 0 {
				setClauses = append(setClauses, "nd2 = COALESCE(nd2, ?)")
				args = append(args, msg.ND2)
			}
			if msg.Score2 != 0 {
				setClauses = append(setClauses, "score2 = COALESCE(score2, ?)")
				args = append(args, msg.Score2)
			}

			args = append(args, existingID)
			query := "UPDATE routines SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
			if _, err := db.Exec(query, args...); err != nil {
				log.Printf("saveEvent update error: %v", err)
			} else {
				routineID = existingID
			}
		} else {
			// No open row found — insert with all available data from the message.
			cols := []string{"server", "apparatus", "competitor", "name", "club", "time_start"}
			args := []any{msg.Server, msg.Apparatus, msg.Competitor, msg.Name, msg.Club, msg.Time}

			cols = append(cols, "time_stop")
			args = append(args, msg.Time)

			hasScore := msg.FinalScore != 0 || msg.DScore != 0 || msg.EScore != 0 || msg.Score1 != 0 ||
				msg.DScore2 != 0 || msg.EScore2 != 0 || msg.Score2 != 0

			if hasScore {
				cols = append(cols, "time_score")
				args = append(args, msg.Time)
			}
			if msg.DScore != 0 {
				cols = append(cols, "d")
				args = append(args, msg.DScore)
			}
			if msg.EScore != 0 {
				cols = append(cols, "e")
				args = append(args, msg.EScore)
			}
			if msg.ND != 0 {
				cols = append(cols, "nd")
				args = append(args, msg.ND)
			}
			if msg.FinalScore != 0 {
				cols = append(cols, "final_score")
				args = append(args, msg.FinalScore)
			}
			if msg.Score1 != 0 {
				cols = append(cols, "score1")
				args = append(args, msg.Score1)
			}
			if msg.DScore2 != 0 {
				cols = append(cols, "d2")
				args = append(args, msg.DScore2)
			}
			if msg.EScore2 != 0 {
				cols = append(cols, "e2")
				args = append(args, msg.EScore2)
			}
			if msg.ND2 != 0 {
				cols = append(cols, "nd2")
				args = append(args, msg.ND2)
			}
			if msg.Score2 != 0 {
				cols = append(cols, "score2")
				args = append(args, msg.Score2)
			}

			placeholders := strings.Repeat("?,", len(cols))
			placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
			query := "INSERT INTO routines (" + strings.Join(cols, ", ") + ") VALUES (" + placeholders + ")"
			result, err := db.Exec(query, args...)
			if err != nil {
				log.Printf("saveEvent insert (no match) error: %v", err)
			} else {
				routineID, _ = result.LastInsertId()
			}
		}
	default:
		log.Printf("saveEvent: unhandled status %q, skipping", msg.Status)
	}
	saveMessage(msg, routineID)
	record, _ := readEvent(routineID)
	hub.broadcast(record)
}

func scanRoutineRow(rows *sql.Rows) (EventMsg, error) {
	var e EventMsg
	var id, timeStop, timeScore, timeScore2 sql.NullInt64
	var d, ev, nd, finalScore, score1, d2, e2, nd2, score2 sql.NullFloat64
	err := rows.Scan(
		&id, &e.Server, &e.Apparatus, &e.Competitor, &e.Name, &e.Club,
		&e.TimeStart, &timeStop, &timeScore, &timeScore2,
		&d, &ev, &nd, &finalScore, &score1, &d2, &e2, &nd2, &score2,
	)
	if err != nil {
		return e, err
	}
	if id.Valid {
		e.ID = &id.Int64
	}
	if timeStop.Valid {
		e.TimeStop = &timeStop.Int64
	}
	if timeScore.Valid {
		e.TimeScore = &timeScore.Int64
	}
	if timeScore2.Valid {
		e.TimeScore2 = &timeScore2.Int64
	}
	if d.Valid {
		e.D = d.Float64
	}
	if ev.Valid {
		e.E = ev.Float64
	}
	if nd.Valid {
		e.ND = nd.Float64
	}
	if finalScore.Valid {
		e.FinalScore = finalScore.Float64
	}
	if score1.Valid {
		e.Score1 = score1.Float64
	}
	if d2.Valid {
		e.D2 = d2.Float64
	}
	if e2.Valid {
		e.E2 = e2.Float64
	}
	if nd2.Valid {
		e.ND2 = nd2.Float64
	}
	if score2.Valid {
		e.Score2 = score2.Float64
	}
	switch {
	case e.TimeScore != nil || e.TimeScore2 != nil:
		e.Status = "scoring"
	case e.TimeStop != nil:
		e.Status = "stopped"
	default:
		e.Status = "competing"
	}
	return e, nil
}

const routineColumns = `id, server, apparatus, competitor, name, club,
	time_start, time_stop, time_score, time_score2,
	d, e, nd, final_score, score1, d2, e2, nd2, score2`

func readEvents() ([]EventMsg, error) {
	rows, err := db.Query(`SELECT ` + routineColumns + ` FROM routines ORDER BY time_start ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []EventMsg
	for rows.Next() {
		e, err := scanRoutineRow(rows)
		if err != nil {
			log.Println("readEvents scan error:", err)
			continue
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
func readEvent(routineID int64) (EventMsg, error) {
	rows, err := db.Query(`SELECT `+routineColumns+` FROM routines WHERE id = ? limit 1`, routineID)
	if err != nil {
		return EventMsg{}, err
	}
	defer rows.Close()
	var event EventMsg
	for rows.Next() {
		e, err := scanRoutineRow(rows)
		if err != nil {
			log.Println("readEvent scan error:", err)
			continue
		}
		event = e
	}
	return event, rows.Err()
}
func readScoredEvents() ([]EventMsg, error) {
	rows, err := db.Query(`SELECT ` + routineColumns + `
		FROM routines WHERE final_score IS NOT NULL ORDER BY time_start ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []EventMsg
	for rows.Next() {
		e, err := scanRoutineRow(rows)
		if err != nil {
			log.Println("readScoredEvents scan error:", err)
			continue
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
