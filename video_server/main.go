package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"embed"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grandcat/zeroconf"
	"github.com/mdp/qrterminal/v3"
	"github.com/skip2/go-qrcode"
	_ "modernc.org/sqlite"
)

// ─── Embedded static files ───────────────────────────────────────────────────

//go:embed static
var staticFiles embed.FS

// ─── Config ──────────────────────────────────────────────────────────────────

const (
	webPort       = 3000 // HTTPS — camera page (requires getUserMedia)
	httpPort      = 3001 // HTTP  — overview and all API endpoints
	keypadUDPPort = 51520
	ipadUDPPort   = 51521
	uploadDir     = "EventData/videos"
	dbPath        = "EventData/events.db"
	certFile      = "cert.crt"
	keyFile       = "key.pem"
)

// ─── Types ───────────────────────────────────────────────────────────────────

type EventMsg struct {
	ID         int64   `json:"id"`
	Server     string  `json:"server"`
	Apparatus  string  `json:"apparatus"`
	Competitor string  `json:"competitor"`
	Name       string  `json:"name"`
	Club       string  `json:"club"`
	TimeStart  int64   `json:"time_start"`
	TimeStop   *int64  `json:"time_stop,omitempty"`
	TimeScore  *int64  `json:"time_score,omitempty"`
	TimeScore2 *int64  `json:"time_score2,omitempty"`
	D          float64 `json:"d,omitempty"`
	E          float64 `json:"e,omitempty"`
	ND         float64 `json:"nd,omitempty"`
	FinalScore float64 `json:"final_score,omitempty"`
	Score1     float64 `json:"score1,omitempty"`
	D2         float64 `json:"d2,omitempty"`
	E2         float64 `json:"e2,omitempty"`
	ND2        float64 `json:"nd2,omitempty"`
	Score2     float64 `json:"score2,omitempty"`
	// Status is not stored in the DB; derived from which time fields are populated.
	// It is set on reads and drives saveEvent logic on writes.
	Status string `json:"status"`
}

type VideoFile struct {
	CameraDesc string `json:"camera_desc"`
	Length     int64  `json:"length"`
	EndTime    int64  `json:"end_time"`
	StartTime  int64  `json:"start_time"`
	Filename   string `json:"filename"`
}

// ─── WebSocket Hub ────────────────────────────────────────────────────────────

type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
}

var hub = &Hub{clients: make(map[*websocket.Conn]bool)}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (h *Hub) register(conn *websocket.Conn) {
	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()
}

func (h *Hub) unregister(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

func (h *Hub) broadcast(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for conn := range h.clients {
		_ = conn.WriteMessage(websocket.TextMessage, data)
	}
}

// ─── Apparatus map ────────────────────────────────────────────────────────────

var apparatus = map[string]string{
	"VT": "Vault",
	"UB": "Bars",
	"BB": "Beam",
	"FX": "Floor",
	"1":  "Vault",
	"2":  "Bars",
	"3":  "Beam",
	"4":  "Floor",
}

// ─── IP helpers ───────────────────────────────────────────────────────────────

func getIPAddresses() map[string][]string {
	result := make(map[string][]string)
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		want := net.FlagUp | net.FlagBroadcast
		if iface.Flags&want != want {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				result[iface.Name] = append(result[iface.Name], ip4.String())
			}
		}
	}
	return result
}

func firstIP() string {
	addrs := getIPAddresses()
	keys := make([]string, 0, len(addrs))
	for k := range addrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "localhost"
	}
	ips := addrs[keys[0]]
	if len(ips) == 0 {
		return "localhost"
	}
	return ips[0]
}

// ─── TLS certificate ──────────────────────────────────────────────────────────

// loadOrCreateCert loads the cert and key from disk, generating a new
// self-signed certificate if either file is missing. The cert intentionally
// has no IP SANs — it is valid for any address — so it survives the server
// getting a different IP between runs. Clients will see a browser warning on
// first connection and need to click through once per device.
func loadOrCreateCert() tls.Certificate {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err == nil {
		log.Printf("Loaded TLS certificate from %s", certFile)
		return cert
	}

	log.Printf("No certificate found (%v) — generating a new self-signed certificate", err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("Failed to generate private key: %v", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "WAG-Video-Review"},
		Issuer:       pkix.Name{CommonName: "Alan Keating"},
		NotBefore:    time.Now().Add(-time.Minute), // small backdate for clock skew
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// No IP SANs — valid for any address the server happens to have.
		// Browsers will warn once; users click "Advanced → proceed" to accept.
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		log.Fatalf("Failed to create certificate: %v", err)
	}

	// Write cert
	certOut, err := os.Create(certFile)
	if err != nil {
		log.Fatalf("Failed to write cert file: %v", err)
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certOut.Close()

	// Write key
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Fatalf("Failed to marshal private key: %v", err)
	}
	keyOut, err := os.Create(keyFile)
	if err != nil {
		log.Fatalf("Failed to write key file: %v", err)
	}
	os.Chmod(keyFile, 0600)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	keyOut.Close()

	log.Printf("Generated new self-signed certificate → %s", certFile)

	cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to load generated certificate: %v", err)
	}
	return cert
}

// ─── Event persistence (SQLite) ───────────────────────────────────────────────

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

func saveEvent(msg EventMsg) {
	switch msg.Status {
	case "competing":
		_, err := db.Exec(`INSERT INTO routines
			(server, apparatus, competitor, name, club, time_start)
			VALUES (?,?,?,?,?,?)`,
			msg.Server, msg.Apparatus, msg.Competitor,
			msg.Name, msg.Club, msg.TimeStart,
		)
		if err != nil {
			log.Println("saveEvent insert error:", err)
		}

	case "stopped", "scoring":
		const tenMin = int64(10 * 60 * 1000)
		windowStart := msg.TimeStart - tenMin
		now := msg.TimeStart

		var existingID int64
		err := db.QueryRow(`SELECT id FROM routines
			WHERE competitor = ? AND apparatus = ? AND server = ?
			  AND time_stop  IS NULL
			  AND time_score IS NULL
			  AND time_start >= ?
			ORDER BY time_start DESC LIMIT 1`,
			msg.Competitor, msg.Apparatus, msg.Server, windowStart,
		).Scan(&existingID)

		if err == nil {
			// Build SET clause dynamically.
			//
			// time_stop  — always set (routine has ended); COALESCE preserves any
			//              value already written by a prior update.
			// time_score — also set when the message carries score data.
			// All non-zero score fields from the message are written too.
			setClauses := []string{"time_stop = COALESCE(time_stop, ?)"}
			args := []any{now}

			hasScore := msg.FinalScore != 0 || msg.D != 0 || msg.E != 0 || msg.Score1 != 0 ||
				msg.D2 != 0 || msg.E2 != 0 || msg.Score2 != 0

			if hasScore {
				scoreTime := now
				if msg.TimeScore != nil {
					scoreTime = *msg.TimeScore
				}
				setClauses = append(setClauses, "time_score = COALESCE(time_score, ?)")
				args = append(args, scoreTime)
			}
			if msg.D != 0 {
				setClauses = append(setClauses, "d = ?")
				args = append(args, msg.D)
			}
			if msg.E != 0 {
				setClauses = append(setClauses, "e = ?")
				args = append(args, msg.E)
			}
			if msg.ND != 0 {
				setClauses = append(setClauses, "nd = ?")
				args = append(args, msg.ND)
			}
			if msg.FinalScore != 0 {
				setClauses = append(setClauses, "final_score = ?")
				args = append(args, msg.FinalScore)
			}
			if msg.Score1 != 0 {
				setClauses = append(setClauses, "score1 = ?")
				args = append(args, msg.Score1)
			}
			if msg.D2 != 0 {
				setClauses = append(setClauses, "d2 = ?")
				args = append(args, msg.D2)
			}
			if msg.E2 != 0 {
				setClauses = append(setClauses, "e2 = ?")
				args = append(args, msg.E2)
			}
			if msg.ND2 != 0 {
				setClauses = append(setClauses, "nd2 = ?")
				args = append(args, msg.ND2)
			}
			if msg.Score2 != 0 {
				setClauses = append(setClauses, "score2 = ?")
				args = append(args, msg.Score2)
			}

			args = append(args, existingID)
			query := "UPDATE routines SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
			if _, err := db.Exec(query, args...); err != nil {
				log.Printf("saveEvent update error: %v", err)
			}
		} else {
			// No open row found — insert a skeleton row with just time_start.
			// Do not set time_stop: we have no matching "competing" record so
			// this arrival is out-of-order; leave the row open rather than
			// immediately closing it.
			if _, err := db.Exec(`INSERT INTO routines
				(server, apparatus, competitor, name, club, time_start)
				VALUES (?,?,?,?,?,?)`,
				msg.Server, msg.Apparatus, msg.Competitor,
				msg.Name, msg.Club, msg.TimeStart,
			); err != nil {
				log.Printf("saveEvent insert (no match) error: %v", err)
			}
		}

	default:
		log.Printf("saveEvent: unhandled status %q, skipping", msg.Status)
	}
}

func scanRoutineRow(rows *sql.Rows) (EventMsg, error) {
	var e EventMsg
	var timeStop, timeScore, timeScore2 sql.NullInt64
	var d, ev, nd, finalScore, score1, d2, e2, nd2, score2 sql.NullFloat64
	err := rows.Scan(
		&e.ID, &e.Server, &e.Apparatus, &e.Competitor, &e.Name, &e.Club,
		&e.TimeStart, &timeStop, &timeScore, &timeScore2,
		&d, &ev, &nd, &finalScore, &score1, &d2, &e2, &nd2, &score2,
	)
	if err != nil {
		return e, err
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
	rows, err := db.Query(`SELECT ` + routineColumns + `
		FROM routines ORDER BY time_start ASC`)
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

// ─── UDP: Keypad ──────────────────────────────────────────────────────────────

func csvSplit(s string) []string {
	var fields []string
	inQuote := false
	start := 0
	for i, ch := range s {
		if ch == '"' {
			inQuote = !inQuote
		} else if ch == ',' && !inQuote {
			fields = append(fields, s[start:i])
			start = i + 1
		}
	}
	fields = append(fields, s[start:])
	return fields
}

func listenKeypad() {
	addr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", keypadUDPPort))
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Fatalf("Keypad UDP listen error: %v", err)
	}
	log.Printf("Listening for keypads on UDP %s", addr)
	buf := make([]byte, 65535)
	for {
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Println("Keypad UDP read error:", err)
			continue
		}
		raw := string(buf[:n])
		parts := csvSplit(raw)

		stripQuotes := func(s string) string { return strings.ReplaceAll(s, `"`, "") }
		get := func(i int) string {
			if i < len(parts) {
				return parts[i]
			}
			return ""
		}

		cmd := get(0)
		var status string
		switch cmd {
		case "PODIUM-CLEAR", "PODIUM-SCORE":
			status = "stopped"
		case "PODIUM-STATUS":
			switch get(1) {
			case "0":
				status = "scoring"
			case "1":
				status = "competing"
			default:
				status = "ready"
			}
		default:
			status = "unknown"
		}

		var app string
		if cmd == "PODIUM-STATUS" {
			app = apparatus[get(2)]
		} else {
			app = apparatus[get(1)]
		}

		var competitor, name, club string
		if cmd == "PODIUM-STATUS" {
			competitor = get(3)
			name = strings.TrimSpace(stripQuotes(get(4)) + " " + stripQuotes(get(5)))
			club = stripQuotes(get(6))
		}

		msg := EventMsg{
			Server:     raddr.IP.String(),
			Status:     status,
			Apparatus:  app,
			Competitor: competitor,
			Name:       name,
			Club:       club,
			TimeStart:  time.Now().UnixMilli(),
		}

		saveEvent(msg)
		hub.broadcast(msg)

		compStr := ""
		if competitor != "" {
			compStr = fmt.Sprintf(" (%s: %s %s)", competitor, name, club)
		}
		log.Printf("%s @ %s - %s: %s%s",
			msg.Server, time.Now().Format("15:04:05"), msg.Apparatus, msg.Status, compStr)
	}
}

// ─── UDP: iPads (XML) ─────────────────────────────────────────────────────────

var (
	reAttr = regexp.MustCompile(`(\w+)="([^"]*)"`)
	reGym  = regexp.MustCompile(`<Gym>(.*?)</Gym>`)
	reRoot = regexp.MustCompile(`<(\w+)`)
)

type xmlParsed struct {
	tag   string
	attrs map[string]string
	gym   string
}

func parseXML(data []byte) xmlParsed {
	s := string(data)
	result := xmlParsed{attrs: make(map[string]string)}
	if m := reRoot.FindStringSubmatch(s); m != nil {
		result.tag = m[1]
	}
	for _, m := range reAttr.FindAllStringSubmatch(s, -1) {
		result.attrs[m[1]] = m[2]
	}
	if m := reGym.FindStringSubmatch(s); m != nil {
		result.gym = m[1]
	}
	return result
}

func listenIPads() {
	addr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", ipadUDPPort))
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Fatalf("iPad UDP listen error: %v", err)
	}
	log.Printf("Listening for iKeypads on UDP %s", addr)
	buf := make([]byte, 65535)
	for {
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Println("iPad UDP read error:", err)
			continue
		}
		parsed := parseXML(buf[:n])

		status := "stopped"
		if parsed.tag == "NowUp" {
			status = "competing"
		}

		msg := EventMsg{
			Server:     raddr.IP.String(),
			Status:     status,
			Apparatus:  parsed.attrs["Event"],
			Competitor: parsed.attrs["Num"],
			Name:       strings.TrimSpace(parsed.attrs["FName"] + " " + parsed.attrs["LName"]),
			Club:       parsed.gym,
			TimeStart:  time.Now().UnixMilli(),
		}

		saveEvent(msg)
		hub.broadcast(msg)

		log.Printf("%s @ %s - %s: %s (%s: %s %s)",
			msg.Server, time.Now().Format("15:04:05"),
			msg.Apparatus, msg.Status,
			msg.Competitor, msg.Name, msg.Club)
	}
}

// ─── HTTP Handlers ────────────────────────────────────────────────────────────

func handleVideoList(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	var files []VideoFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		parts := strings.SplitN(name, "!", 3)
		if len(parts) < 3 {
			log.Println("Couldn't parse video filename:", name)
			continue
		}
		endTime, _ := strconv.ParseInt(strings.Split(parts[1], ".")[0], 10, 64)
		lengthStr := parts[2]
		if idx := strings.Index(lengthStr, "."); idx >= 0 {
			lengthStr = lengthStr[:idx]
		}
		length, _ := strconv.ParseInt(lengthStr, 10, 64)
		vf := VideoFile{
			CameraDesc: parts[0],
			Length:     length,
			EndTime:    endTime,
			StartTime:  endTime - length,
			Filename:   name,
		}
		files = append(files, vf)
	}

	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if a.CameraDesc != b.CameraDesc {
			return a.CameraDesc < b.CameraDesc
		}
		return a.StartTime < b.StartTime
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func handleCameraList(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	seen := make(map[string]bool)
	var cameras []string
	for _, e := range entries {
		cam := strings.SplitN(e.Name(), "!", 2)[0]
		if !seen[cam] {
			seen[cam] = true
			cameras = append(cameras, cam)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cameras)
}

func handleUploadChunked(w http.ResponseWriter, r *http.Request) {
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	filename := r.URL.Query().Get("name")
	filename = filepath.Base(filename)
	uploadPath := filepath.Join(uploadDir, filename)
	f, err := os.OpenFile(uploadPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer f.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			f.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	w.WriteHeader(200)
	w.Write([]byte("Upload complete!"))
}

func handleVideoServe(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/video/")
	http.ServeFile(w, r, filepath.Join(uploadDir, filename))
}

func handleEventList(w http.ResponseWriter, r *http.Request) {
	events, err := readEvents()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

func handleScoreList(w http.ResponseWriter, r *http.Request) {
	events, err := readScoredEvents()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

func handleQR(w http.ResponseWriter, r *http.Request, urlPath string, scheme string) {
	ip := firstIP()
	port := webPort
	if scheme == "http" {
		port = httpPort
	}
	target := fmt.Sprintf("%s://%s:%d%s", scheme, ip, port, urlPath)
	png, err := qrcode.Encode(target, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	encoded := fmt.Sprintf("data:image/png;base64,%s", encodeBase64(png))
	w.Write([]byte(encoded))
}

func encodeBase64(data []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, (len(data)+2)/3*4)
	for i := 0; i < len(data); i += 3 {
		var b [3]byte
		copy(b[:], data[i:])
		n := len(data) - i
		if n > 3 {
			n = 3
		}
		v := int(b[0])<<16 | int(b[1])<<8 | int(b[2])
		j := i / 3 * 4
		out[j] = chars[v>>18&0x3f]
		out[j+1] = chars[v>>12&0x3f]
		if n < 2 {
			out[j+2] = '='
		} else {
			out[j+2] = chars[v>>6&0x3f]
		}
		if n < 3 {
			out[j+3] = '='
		} else {
			out[j+3] = chars[v&0x3f]
		}
	}
	return string(out)
}

func handleIP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(getIPAddresses())
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	var body interface{}
	json.NewDecoder(r.Body).Decode(&body)
	log.Println(body)
	w.WriteHeader(200)
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	hub.register(conn)
	defer func() {
		hub.unregister(conn)
		conn.Close()
	}()
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// ─── Filtered logger ─────────────────────────────────────────────────────────

// tlsErrorFilter silently drops the "TLS handshake error: unknown certificate"
// lines that appear when a browser rejects the self-signed cert before the user
// has clicked through the warning. Everything else passes through unchanged.
type tlsErrorFilter struct{ w io.Writer }

func (f tlsErrorFilter) Write(p []byte) (n int, err error) {
	if strings.Contains(string(p), "TLS handshake error") &&
		strings.Contains(string(p), "unknown certificate") {
		return len(p), nil
	}
	return f.w.Write(p)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	os.MkdirAll(uploadDir, 0755)
	initDB()
	defer db.Close()

	go listenKeypad()
	go listenIPads()

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create static sub-FS: %v", err)
	}
	staticHandler := http.FileServer(http.FS(staticFS))

	// Shared routes — registered on both muxes below.
	registerShared := func(mux *http.ServeMux) {
		mux.HandleFunc("/socket.io/", handleWS)
		mux.HandleFunc("/ws", handleWS)
		mux.HandleFunc("/ip", handleIP)
		mux.HandleFunc("/events", handleEvents)
		mux.HandleFunc("/videolist", handleVideoList)
		mux.HandleFunc("/video_list", handleVideoList)
		mux.HandleFunc("/cameralist", handleCameraList)
		mux.HandleFunc("/uploadChunked", handleUploadChunked)
		mux.HandleFunc("/video/", handleVideoServe)
		mux.HandleFunc("/eventlist", handleEventList)
		mux.HandleFunc("/scorelist", handleScoreList)
		mux.HandleFunc("/cameraQR", func(w http.ResponseWriter, r *http.Request) { handleQR(w, r, "/", "https") })
		mux.HandleFunc("/overviewQR", func(w http.ResponseWriter, r *http.Request) { handleQR(w, r, "/overview", "http") })
	}

	// ── HTTPS mux: camera page only (getUserMedia requires a secure context) ──
	httpsMux := http.NewServeMux()
	registerShared(httpsMux)
	httpsMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/camera":
			r.URL.Path = "/camera.html"
		case "/overview":
			r.URL.Path = "/overview.html"
		default:
			// All other static assets (JS, CSS, fonts, etc.) still need to be
			// served over HTTPS so the camera page can load them.
		}
		staticHandler.ServeHTTP(w, r)
	})

	// ── HTTP mux: overview and everything else ────────────────────────────────
	httpMux := http.NewServeMux()
	registerShared(httpMux)
	httpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/overview", "/":
			r.URL.Path = "/overview.html"
		default:
		}
		staticHandler.ServeHTTP(w, r)
	})

	tlsCert := loadOrCreateCert()
	httpsServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", webPort),
		Handler: httpsMux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		},
		ErrorLog: log.New(tlsErrorFilter{log.Writer()}, "", log.LstdFlags),
	}
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", httpPort),
		Handler: httpMux,
	}

	addrs := getIPAddresses()
	keys := make([]string, 0, len(addrs))
	for k := range addrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	mDNSserver, _ := zeroconf.Register("WAG-Video-Review", "_http._tcp", "local.", httpPort, nil, nil)
	defer mDNSserver.Shutdown()

	if len(keys) == 1 {
		ip := addrs[keys[0]][0]
		cameraAddr := fmt.Sprintf("https://%s:%d/", ip, webPort)
		overviewAddr := fmt.Sprintf("http://%s:%d/overview", ip, httpPort)
		log.Printf("Camera  (HTTPS): %s", cameraAddr)
		log.Printf("Overview (HTTP): %s", overviewAddr)
		log.Printf("\nConnect camera devices to %s\n", cameraAddr)
		qrterminal.GenerateHalfBlock(cameraAddr, qrterminal.L, os.Stdout)
	} else {
		for _, k := range keys {
			ip := addrs[k][0]
			log.Printf("Camera  (HTTPS) on %s: https://%s:%d/", k, ip, webPort)
			log.Printf("Overview (HTTP) on %s: http://%s:%d/overview", k, ip, httpPort)
		}
	}

	log.Println("Note: browsers will warn about the self-signed certificate on first camera connection.")
	log.Println("Click 'Advanced → proceed' to accept it. You only need to do this once per device.")
	log.Println("Press ctrl-c to quit")

	go func() {
		if err := httpServer.ListenAndServe(); err != nil {
			log.Fatal("HTTP server error:", err)
		}
	}()

	if err := httpsServer.ListenAndServeTLS("", ""); err != nil {
		log.Fatal("HTTPS server error:", err)
	}
}
