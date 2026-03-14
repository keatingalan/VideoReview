//go:build windows
// +build windows

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)


// ProScoreMessage is the unified structure sent to the HTTP endpoint for all ports.
type ProScoreMessage struct {
	Time        int64  `json:"time"`
	Server      string `json:"server"`
	Status      string `json:"status"`
	Apparatus   string `json:"apparatus"`
	Competitor  string `json:"competitor"`
	Name        string `json:"Name"`
	Club        string `json:"club"`
	DScore      float64 `json:"Dscore"`
	EScore		float64
	ND			float64
	FinalScore	float64
	Score1		float64
	DScore2      float64 
	EScore2		float64
	ND2			float64
	Score2		float64
	FullMessage any `json:"fullMessage"`
}

type Stats struct {
	mu               sync.Mutex
	messagesRx       int
	messagesSent     int
	messagesFailed   int
	queueSize        int
	startTime        time.Time
	consecutiveFails int
}

var stats = Stats{startTime: time.Now()}

var (
	httpEndpoint      string
	endpointMu        sync.RWMutex
	promptingEndpoint bool
	promptingMu       sync.Mutex
)

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

// ── GUI widgets ───────────────────────────────────────────────────────────────

var (
	mainWindow     *walk.MainWindow
	lblStatusVal   *walk.Label
	lblUptimeVal   *walk.Label
	lblEndpointVal *walk.Label
	lblRxVal       *walk.Label
	lblSentVal     *walk.Label
	lblFailedVal   *walk.Label
	lblQueueVal    *walk.Label
	lblRateVal     *walk.Label
	urlEdit        *walk.LineEdit
	logEdit        *walk.TextEdit
)

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	var err error

	err = MainWindow{
		AssignTo: &mainWindow,
		Title:    "Capture ProScore ScoreGen Messages",
		//Icon:     1, //embedded in .syso file
		MinSize: Size{Width: 660, Height: 540},
		Size:    Size{Width: 660, Height: 540},
		Layout:  VBox{Margins: Margins{Left: 8, Top: 8, Right: 8, Bottom: 8}, Spacing: 6},
		Children: []Widget{

			// ── Stats group ──────────────────────────────────────────────
			GroupBox{
				Title:  "Status",
				Layout: Grid{Columns: 4, Spacing: 10},
				Children: []Widget{
					Label{Text: "Status:"},
					Label{AssignTo: &lblStatusVal, Text: "Starting…", ColumnSpan: 3},

					Label{Text: "Uptime:"},
					Label{AssignTo: &lblUptimeVal, Text: "00:00", ColumnSpan: 3},

					//Label{Text: "Endpoint:"},
					//Label{AssignTo: &lblEndpointVal, Text: "—", ColumnSpan: 3},

					Label{Text: "Received:"},
					Label{AssignTo: &lblRxVal, Text: "0"},
					Label{Text: "Sent:"},
					Label{AssignTo: &lblSentVal, Text: "0"},

					Label{Text: "Failed:"},
					Label{AssignTo: &lblFailedVal, Text: "0"},
					Label{Text: "Queue:"},
					Label{AssignTo: &lblQueueVal, Text: "0"},

					Label{Text: "Success Rate:"},
					Label{AssignTo: &lblRateVal, Text: "N/A", ColumnSpan: 3},
				},
			},

			// ── Endpoint row ─────────────────────────────────────────────
			Composite{
				Layout: HBox{Spacing: 4},
				Children: []Widget{
					LineEdit{
						AssignTo: &urlEdit,
						ReadOnly: true,
						Text:     "Not configured",
						MaxSize:  Size{Height: 24},
					},
					PushButton{
						Text:    "Change Endpoint",
						MaxSize: Size{Width: 130},
						OnClicked: func() {
							go doChangeEndpoint()
						},
					},
					PushButton{
						Text:    "Search mDNS",
						MaxSize: Size{Width: 110},
						OnClicked: func() {
							go doMDNS()
						},
					},
				},
			},

			// ── Log ──────────────────────────────────────────────────────
			GroupBox{
				Title:  "Event Log",
				Layout: VBox{},
				Children: []Widget{
					TextEdit{
						AssignTo: &logEdit,
						ReadOnly: true,
						VScroll:  true,
						MinSize:  Size{Height: 260},
					},
				},
			},
		},
	}.Create()
	if err != nil {
		walk.MsgBox(nil, "Fatal Error", err.Error(), walk.MsgBoxIconError)
		return
	}

	mainWindow.SetIcon(walk.IconApplication())

	// Kick off background work
	go startApp()

	// Stats refresh ticker — must update UI on the GUI goroutine
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if mainWindow == nil {
				continue
			}
			mainWindow.Synchronize(refreshUI)
		}
	}()

	mainWindow.Run()
}

// ── UI helpers (always called on GUI goroutine via Synchronize) ───────────────

func refreshUI() {
	stats.mu.Lock()
	rx := stats.messagesRx
	sent := stats.messagesSent
	failed := stats.messagesFailed
	queue := stats.queueSize
	uptime := time.Since(stats.startTime)
	cf := stats.consecutiveFails
	stats.mu.Unlock()

	rate := "N/A"
	if rx > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(sent)/float64(rx)*100)
	}

	status := "✓  Running normally"
	if cf > 5 {
		status = fmt.Sprintf("⚠  %d consecutive failures", cf)
	} else if queue > 10 {
		status = fmt.Sprintf("⚠  Queue building: %d", queue)
	}

	lblStatusVal.SetText(status)
	lblUptimeVal.SetText(formatDuration(uptime))
	lblRxVal.SetText(fmt.Sprintf("%d", rx))
	lblSentVal.SetText(fmt.Sprintf("%d", sent))
	lblFailedVal.SetText(fmt.Sprintf("%d", failed))
	lblQueueVal.SetText(fmt.Sprintf("%d", queue))
	lblRateVal.SetText(rate)

	endpointMu.RLock()
	ep := httpEndpoint
	endpointMu.RUnlock()
	if urlEdit != nil {
		urlEdit.SetText(ep)
	}
}

func appendLog(line string) {
	ts := time.Now().Format("15:04:05")
	entry := ts + "  " + line + "\r\n"
	if mainWindow != nil {
		mainWindow.Synchronize(func() {
			if logEdit != nil {
				logEdit.AppendText(entry)
			}
		})
	}
}

func updateURLDisplay() {
	endpointMu.RLock()
	ep := httpEndpoint
	endpointMu.RUnlock()
	if mainWindow != nil {
		mainWindow.Synchronize(func() {
			if urlEdit != nil {
				urlEdit.SetText(ep)
			}
		})
	}
}

// ── Button handlers (run in goroutines) ──────────────────────────────────────

func doChangeEndpoint() {
	endpointMu.RLock()
	cur := httpEndpoint
	endpointMu.RUnlock()

	url := guiInputBox("Change Endpoint", "Enter new HTTP endpoint URL:", cur)
	if url == "" {
		return
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		guiAlert("Invalid URL", "URL must start with http:// or https://")
		return
	}
	endpointMu.Lock()
	httpEndpoint = url
	endpointMu.Unlock()
	stats.mu.Lock()
	stats.consecutiveFails = 0
	stats.mu.Unlock()
	updateURLDisplay()
	appendLog("Endpoint updated: " + url)
}

func doMDNS() {
	appendLog("Searching for mDNS services (5s)…")
	ep := searchMDNS()
	if ep == "" {
		appendLog("No mDNS service found.")
		return
	}
	endpointMu.Lock()
	httpEndpoint = ep
	endpointMu.Unlock()
	stats.mu.Lock()
	stats.consecutiveFails = 0
	stats.mu.Unlock()
	updateURLDisplay()
	appendLog("Endpoint set via mDNS: " + ep)
}

// ── Walk dialog helpers ───────────────────────────────────────────────────────

// guiInputBox shows a modal input dialog and returns the entered text (or "" on cancel).
func guiInputBox(title, prompt, defaultVal string) string {
	result := defaultVal
	cancelled := true

	var dlg *walk.Dialog
	var edit *walk.LineEdit

	Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: new(*walk.PushButton),
		CancelButton:  new(*walk.PushButton),
		MinSize:       Size{Width: 420, Height: 140},
		Layout:        VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 6},
		Children: []Widget{
			Label{Text: prompt},
			LineEdit{AssignTo: &edit, Text: defaultVal},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					HSpacer{},
					PushButton{
						Text: "OK",
						OnClicked: func() {
							result = edit.Text()
							cancelled = false
							dlg.Accept()
						},
					},
					PushButton{
						Text: "Cancel",
						OnClicked: func() {
							dlg.Cancel()
						},
					},
				},
			},
		},
	}.Create(mainWindow)

	dlg.Run()

	if cancelled {
		return ""
	}
	return result
}

func guiAlert(title, message string) {
	mainWindow.Synchronize(func() {
		walk.MsgBox(mainWindow, title, message, walk.MsgBoxIconWarning)
	})
}

func guiYesNo(title, message string) bool {
	result := make(chan bool, 1)
	mainWindow.Synchronize(func() {
		r := walk.MsgBox(mainWindow, title, message, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion)
		result <- (r == walk.DlgCmdYes)
	})
	return <-result
}

// ── Endpoint setup ────────────────────────────────────────────────────────────

func bindUDP(port int) (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, fmt.Errorf("resolve port %d: %w", port, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind port %d: %w", port, err)
	}
	return conn, nil
}

func startApp() {
	appendLog("Waiting for endpoint configuration…")
	httpEndpoint = guiGetHTTPEndpoint()
	updateURLDisplay()
	appendLog("Endpoint set: " + httpEndpoint)

	// Enumerate this machine's non-loopback IPs once at startup
	if err := initLocalIPs(); err != nil {
		appendLog("Warning: could not enumerate local IPs: " + err.Error())
	} else {
		appendLog(fmt.Sprintf("Accepting from local IPs: %v", localIPs))
	}

	// Bind all ports before launching any listeners — fail fast if any port is unavailable
	ports := []int{51520, 51521, 23467}
	conns := make([]*net.UDPConn, len(ports))
	for i, port := range ports {
		conn, err := bindUDP(port)
		if err != nil {
			// Close any already-opened sockets
			for j := 0; j < i; j++ {
				conns[j].Close()
			}
			errMsg := fmt.Sprintf("Could not bind port %d:\n%v", port, err)
			appendLog("ERROR: " + errMsg)
			guiAlert("Fatal Error", errMsg)
			// Update status label to show failure
			mainWindow.Synchronize(func() {
				lblStatusVal.SetText(fmt.Sprintf("⚠  Failed to bind port %d", port))
			})
			return
		}
		conns[i] = conn
		appendLog(fmt.Sprintf("Listening on 0.0.0.0:%d", port))
	}

	go monitorFailures()
	go func() {
		ticker := time.NewTicker(90 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := initLocalIPs(); err != nil {
				appendLog("IP refresh error: " + err.Error())
			} else {
				appendLog(fmt.Sprintf("Local IPs refreshed: %v", localIPs))
			}
		}
	}()

	// Launch all listeners — each conn is closed when startApp returns via defer
	for _, conn := range conns {
		c := conn // capture loop variable
		defer c.Close()
		go listenUDP(c)
	}

	// Block until signalled (listeners run as goroutines; keep startApp alive)
	select {}
}

func guiGetHTTPEndpoint() string {
	if guiYesNo("Endpoint Setup", "Search for an HTTP endpoint via mDNS?\n\nYes = Search mDNS\nNo = Enter URL manually") {
		appendLog("Searching mDNS (5s)…")
		if ep := searchMDNS(); ep != "" {
			return ep
		}
		appendLog("No mDNS service found — please enter URL manually.")
	}
	for {
		url := guiInputBox("HTTP Endpoint", "Enter HTTP endpoint URL:", "http://")
		if url == "" {
			if guiYesNo("No Endpoint", "No URL entered. Try mDNS search instead?") {
				if ep := searchMDNS(); ep != "" {
					return ep
				}
			}
			continue
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			guiAlert("Invalid URL", "URL must start with http:// or https://")
			continue
		}
		return url
	}
}

// ── mDNS ──────────────────────────────────────────────────────────────────────

func searchMDNS() string {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		appendLog("mDNS error: " + err.Error())
		return ""
	}

	entries := make(chan *zeroconf.ServiceEntry)
	var found []*zeroconf.ServiceEntry
	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for e := range entries {
			found = append(found, e)
			if len(e.AddrIPv4) > 0 {
				appendLog(fmt.Sprintf("  mDNS: %s — %s:%d", e.Instance, e.AddrIPv4[0], e.Port))
			}
		}
	}()

	if err = resolver.Browse(ctx, "_https._tcp", "local.", entries); err != nil {
		appendLog("mDNS browse error: " + err.Error())
		return ""
	}
	<-ctx.Done()
	wg.Wait()

	if len(found) == 0 {
		return ""
	}
	if len(found) == 1 && len(found[0].AddrIPv4) > 0 {
		ep := fmt.Sprintf("https://%s:%d", found[0].AddrIPv4[0], found[0].Port)
		appendLog("mDNS auto-selected: " + ep)
		return ep
	}

	// Multiple results — let user pick
	var lines []string
	for i, s := range found {
		if len(s.AddrIPv4) > 0 {
			lines = append(lines, fmt.Sprintf("%d. %s  (%s:%d)", i+1, s.Instance, s.AddrIPv4[0], s.Port))
		}
	}
	choice := guiInputBox("Select mDNS Service",
		"Multiple services found. Enter number:\n\n"+strings.Join(lines, "\n"), "1")
	var sel int
	fmt.Sscanf(choice, "%d", &sel)
	if sel < 1 || sel > len(found) || len(found[sel-1].AddrIPv4) == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", found[sel-1].AddrIPv4[0], found[sel-1].Port)
}

// ── UDP listener ──────────────────────────────────────────────────────────────

// ── Local IP tracking ────────────────────────────────────────────────────────────────────

var (
	localIPsMu sync.RWMutex
	localIPs   []net.IP
)

// initLocalIPs enumerates all non-loopback unicast IPv4 addresses on this machine.
func initLocalIPs() error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	var ips []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				ips = append(ips, ip4)
			}
		}
	}
	localIPsMu.Lock()
	localIPs = ips
	localIPsMu.Unlock()
	if len(ips) == 0 {
		return fmt.Errorf("no non-loopback IPv4 addresses found")
	}
	return nil
}

// isLocalIP returns true if ip matches one of this machine's non-loopback IPs.
func isLocalIP(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	localIPsMu.RLock()
	defer localIPsMu.RUnlock()
	for _, local := range localIPs {
		if local.Equal(ip4) {
			return true
		}
	}
	return false
}

func listenUDP(conn *net.UDPConn) {
	conn.SetReadBuffer(65536)

	// Destination port is fixed — it's the port this socket is bound to
	destPort := conn.LocalAddr().(*net.UDPAddr).Port

	buffer := make([]byte, 4096)
	oob := make([]byte, 1024) // out-of-band control message buffer

	for {
		n, _, _, src, err := conn.ReadMsgUDP(buffer, oob)
		if err != nil {
			continue
		}
		data := string(buffer[:n])

		//Reject status updates on port 23467?
		if destPort == 23467 && data[0:9] == "SCOREGEN," { //This is a routine status update
			continue
		}
		appendLog(fmt.Sprintf("Pkt: src=%s:%d → dst port=%d - %s", src.IP, src.Port, destPort, data))

		stats.mu.Lock()
		stats.messagesRx++
		stats.queueSize++
		stats.mu.Unlock()

		// Parse message into ProScoreMessage based on port
		var msg ProScoreMessage
		var parseErr error
		switch destPort {
		case 51521:
			// XML from ScoreGen — NowUp or NewScore element
			msg, parseErr = parseXMLMessage(src.IP.String(), data)
		case 51520:
			// CSV PODIUM-* messages
			msg, parseErr = parseCSVMessage(src.IP.String(), data)
		case 23467:
			msg, parseErr = parseCSVMessage(src.IP.String(), data)
		}
		if parseErr != nil {
			appendLog(fmt.Sprintf("Parse error (port %d): %v", destPort, parseErr))
			stats.mu.Lock()
			stats.queueSize--
			stats.messagesFailed++
			stats.consecutiveFails++
			stats.mu.Unlock()
			continue
		}

		go sendToHTTP(msg)
	}
}

// ── Message parsers ──────────────────────────────────────────────────────────

// parseXMLMessage handles port 51520: XML with a NowUp or NewScore root element.
// NowUp → status=competing, NewScore → status=stopped.
func parseXMLMessage(server, data string) (ProScoreMessage, error) {
	// Parse XML into a generic map
	decoder := xml.NewDecoder(strings.NewReader(data))
	root, err := xmlNodeToMap(decoder)
	if err != nil {
		return ProScoreMessage{}, fmt.Errorf("XML parse: %w", err)
	}

	msg := ProScoreMessage{
		Time:        time.Now().UnixMilli(),
		Server:      server,
		FullMessage: root,
	}

	if nowUp, ok := root["NowUp"].(map[string]any); ok {
		attrs := xmlAttrs(nowUp)
		msg.Status = "competing"
		msg.Apparatus = apparatus[attrs["Event"]]
		msg.Competitor = attrs["Num"]
		msg.Name = attrs["FName"] + " " + attrs["LName"]
		msg.Club = xmlStr(nowUp, "Gym")
	} else if newScore, ok := root["NewScore"].(map[string]any); ok {
		attrs := xmlAttrs(newScore)
		msg.Status = "stopped"
		msg.Apparatus = apparatus[attrs["Event"]]
		msg.Competitor = attrs["Num"]
		msg.Name = attrs["FName"] + " " + attrs["LName"]
		msg.Club = xmlStr(newScore, "Gym")
		msg.Level=attrs["Level"]
		msg.FinalScore=attrs["Score"]
		//Try to get the rest of the information via http to ScoreGen
		info,err=GetCompetitorInfoByHTTP(msg.Competitor,1) //Not sure what happens if get wrong group!
		msg.Dscore=info.Start_Value1
		msg.Escore=info.EScore1
		msg.ND=info.Adjust1
		msg.Score1=info.Score1
		msg.Dscore2=info.Start_Value2
		msg.Escore2=info.EScore2
		msg.ND2=info.Adjust2
		msg.Score2=info.Score2
	} else {
		msg.Status = "unknown"
	}

	return msg, nil
}

// xmlAttrs extracts the _attr sub-map from an xmlNodeToMap result as flat strings.
func xmlAttrs(node map[string]any) map[string]string {
	result := map[string]string{}
	if attrs, ok := node["_attr"].(map[string]string); ok {
		for k, v := range attrs {
			result["_"+k] = v
			result[k] = v
		}
	}
	return result
}

// xmlStr returns a string child element value from an xmlNodeToMap node.
func xmlStr(node map[string]any, key string) string {
	if child, ok := node[key].(map[string]any); ok {
		if text, ok := child["_text"].(string); ok {
			return text
		}
	}
	return ""
}

// parseCSVMessage handles port 51521: comma-separated PODIUM-* messages.
// Matches Node.js CSV logic with quoted-field-aware splitting.
// Format examples:
//
//		PODIUM-STATUS,0,VT,42(competitornum),"Jane","Smith","Club"
//		PODIUM-SCORE,VT
//		PODIUM-CLEAR,VT
//	 SCOREGEN-LAST,1,3(apparatus),42(competitornum),"Jane Smith",2(rotation?),2.0(E1),E2,E3,E4,E5,E6,D,E,ND,Final
func parseCSVMessage(server, data string) (ProScoreMessage, error) {
	fields := splitCSV(strings.TrimSpace(data))
	if len(fields) == 0 {
		return ProScoreMessage{}, fmt.Errorf("empty CSV message")
	}

	msg := ProScoreMessage{
		Time:        time.Now().UnixMilli(), //)Now().Format(time.StampMilli),
		Server:      server,
		FullMessage: fields,
	}

	cmd := fields[0]
	switch cmd {
	case "PODIUM-STATUS":
		// fields: cmd, statusCode, apparatus, competitor, firstName, surname, club
		if len(fields) < 2 {
			return ProScoreMessage{}, fmt.Errorf("PODIUM-STATUS too short")
		}
		switch fields[1] {
		case "0":
			msg.Status = "scoring"
		case "1":
			msg.Status = "competing"
		default:
			msg.Status = "ready"
		}
		if len(fields) > 2 {
			msg.Apparatus = apparatus[fields[2]]
		}
		if len(fields) > 3 {
			msg.Competitor = fields[3]
		}
		if len(fields) > 5 {
			msg.Name = stripQuotes(fields[4]) + " " + stripQuotes(fields[5])
		}
		if len(fields) > 6 {
			msg.Club = stripQuotes(fields[6])
		}
		appendLog(fields[1])
	case "PODIUM-SCORE":
		msg.Status = "stopped"
		if len(fields) > 1 {
			msg.Apparatus = apparatus[fields[1]]
		}
		if len(fields) > 2 {
			msg.Competitor = fields[2]
		}
		if len(fields) > 4 {
			msg.Name = stripQuotes(fields[3]) + " " + stripQuotes(fields[4])
		}
		if len(fields) > 5 {
			msg.Club = stripQuotes(fields[5])
		}
		if len(fields) > 7 {
			msg.Score.Final = fields[7]
		}
		
	case "PODIUM-CLEAR":
		msg.Status = "stopped"
		if len(fields) > 1 {
			msg.Apparatus = apparatus[fields[1]]
		}

	//  SCOREGEN-LAST,1,3(apparatus),42(competitornum),"Jane Smith",2(rotation?),2.0(E1),E2,E3,E4,E5,E6,D,E,ND,Final
	case "SCOREGEN-LAST":
		msg.Status = "stopped"
		//Ignoring whatever field 1 is
		if len(fields) > 2 {
			msg.Apparatus = apparatus[fields[2]]
		}
		if len(fields) > 3 {
			msg.Competitor = fields[3]
		}
		if len(fields) > 4 {
			msg.Name = fields[4]
		}
		//Ignoring whatever field 5 is
		if len(fields) > 6 {
			msg.Score.E1 = fields[6]
		}
		if len(fields) > 7 {
			msg.Score.E2 = fields[7]
		}
		if len(fields) > 8 {
			msg.Score.E3 = fields[8]
		}
		if len(fields) > 9 {
			msg.Score.E4 = fields[9]
		}
		if len(fields) > 10 {
			msg.Score.E5 = fields[10]
		}
		if len(fields) > 11 {
			msg.Score.E6 = fields[11]
		}
		if len(fields) > 12 {
			msg.Score.D = fields[12]
		}
		if len(fields) > 13 {
			msg.Score.E = fields[13]
		}
		if len(fields) > 14 {
			msg.Score.ND = fields[14]
		}
		if len(fields) > 15 {
			msg.Score.Final = fields[15]
		}
	default:
		msg.Status = "unknown"
	}

	return msg, nil
}

// splitCSV splits a CSV line on commas that are not inside quoted fields,
// replicating the regex: /,(?=(?:(?:[^"]*"){2})*[^"]*$)/
func splitCSV(s string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case ch == ',' && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	fields = append(fields, cur.String())
	return fields
}

// stripQuotes removes surrounding double-quotes from a field value.
func stripQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, "")
}

// ── XML → JSON ────────────────────────────────────────────────────────────────

func xmlToJSON(xmlData string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(xmlData))
	root, err := xmlNodeToMap(decoder)
	if err != nil {
		return "", fmt.Errorf("XML parse error: %w", err)
	}
	b, err := json.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("JSON encode error: %w", err)
	}
	return string(b), nil
}

func xmlNodeToMap(decoder *xml.Decoder) (map[string]any, error) {
	result := make(map[string]any)
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			child, err := xmlNodeToMap(decoder)
			if err != nil {
				return nil, err
			}
			if len(t.Attr) > 0 {
				attrs := make(map[string]string, len(t.Attr))
				for _, a := range t.Attr {
					attrs[a.Name.Local] = a.Value
				}
				child["_attr"] = attrs
			}
			if existing, ok := result[t.Name.Local]; ok {
				switch v := existing.(type) {
				case []any:
					result[t.Name.Local] = append(v, child)
				default:
					result[t.Name.Local] = []any{v, child}
				}
			} else {
				result[t.Name.Local] = child
			}
		case xml.CharData:
			if text := strings.TrimSpace(string(t)); text != "" {
				result["_text"] = text
			}
		case xml.EndElement:
			return result, nil
		}
	}
	return result, nil
}

// ── HTTP forwarding ───────────────────────────────────────────────────────────

// httpClient skips TLS certificate verification — needed for self-signed certs
var httpClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
	Timeout: 10 * time.Second,
}

func sendToHTTP(msg ProScoreMessage) {
	jsonData, err := json.Marshal(msg)
	if err != nil {
		stats.mu.Lock()
		stats.queueSize--
		stats.messagesFailed++
		stats.consecutiveFails++
		stats.mu.Unlock()
		return
	}

	endpointMu.RLock()
	endpoint := httpEndpoint + "/events"
	endpointMu.RUnlock()

	resp, err := httpClient.Post(endpoint, "application/json", bytes.NewBuffer(jsonData))

	stats.mu.Lock()
	stats.queueSize--
	stats.mu.Unlock()

	if err != nil {
		stats.mu.Lock()
		stats.messagesFailed++
		stats.consecutiveFails++
		stats.mu.Unlock()
		appendLog("Send error: " + err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		stats.mu.Lock()
		stats.messagesSent++
		stats.consecutiveFails = 0
		stats.mu.Unlock()
	} else {
		stats.mu.Lock()
		stats.messagesFailed++
		stats.consecutiveFails++
		stats.mu.Unlock()
		appendLog(fmt.Sprintf("HTTP %d from endpoint", resp.StatusCode))
	}
}

// ── Failure monitor ───────────────────────────────────────────────────────────

func monitorFailures() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		stats.mu.Lock()
		fails := stats.consecutiveFails
		stats.mu.Unlock()

		if fails >= 10 {
			promptingMu.Lock()
			already := promptingEndpoint
			if !already {
				promptingEndpoint = true
			}
			promptingMu.Unlock()
			if !already {
				go promptEndpointChange()
			}
		}
	}
}

func promptEndpointChange() {
	defer func() {
		promptingMu.Lock()
		promptingEndpoint = false
		promptingMu.Unlock()
	}()

	appendLog("⚠  10+ consecutive failures — prompting for new endpoint")

	endpointMu.RLock()
	current := httpEndpoint
	endpointMu.RUnlock()

	if !guiYesNo("Send Failures",
		fmt.Sprintf("10+ consecutive send failures.\nCurrent endpoint: %s\n\nChange endpoint now?", current)) {
		stats.mu.Lock()
		stats.consecutiveFails = 0
		stats.mu.Unlock()
		appendLog("Keeping current endpoint.")
		return
	}

	var newEndpoint string
	if guiYesNo("Change Endpoint", "Search via mDNS?\n\nYes = mDNS\nNo = Enter manually") {
		newEndpoint = searchMDNS()
	}
	if newEndpoint == "" {
		newEndpoint = guiInputBox("New Endpoint", "Enter HTTP endpoint URL:", current)
	}

	if newEndpoint == "" ||
		(!strings.HasPrefix(newEndpoint, "http://") && !strings.HasPrefix(newEndpoint, "https://")) {
		appendLog("Invalid/empty endpoint — keeping current.")
		stats.mu.Lock()
		stats.consecutiveFails = 0
		stats.mu.Unlock()
		return
	}

	endpointMu.Lock()
	httpEndpoint = newEndpoint
	endpointMu.Unlock()
	stats.mu.Lock()
	stats.consecutiveFails = 0
	stats.mu.Unlock()
	updateURLDisplay()
	appendLog("Endpoint updated: " + newEndpoint)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// -- Get scores from the Ipad interface --
type CompetitorInfo struct {
	Num        int
	FirstName  string
	LastName   string
	Gym        string
	Level      string
	Session    string
	Score1     *float64
	StartValue1 *float64
	EScore1    *float64
	Adjust1    *float64
	Score2     *float64
	StartValue2 *float64
	EScore2    *float64
	Adjust2    *float64
}

func nullableFloat(s string) *float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f == -99 {
		return nil
	}
	return &f
}

func parseProScoreResponse(body string) map[string]string {
	fields := make(map[string]string)
	tokens := strings.Split(body, ";")
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		eq := strings.Index(tok, "=")
		if eq == -1 {
			continue
		}
		keypart := tok[:eq]
		valpart := tok[eq+1:]

		// strip :TYPE from key
		key := keypart
		if colon := strings.Index(keypart, ":"); colon != -1 {
			key = keypart[:colon]
			typePart := keypart[colon+1:]
			if typePart == "S" {
				// S=count"value" — extract using length prefix
				if q := strings.Index(valpart, `"`); q != -1 {
					length, err := strconv.Atoi(valpart[:q])
					if err == nil && q+1+length <= len(valpart) {
						valpart = valpart[q+1 : q+1+length]
					}
				}
			}
		}
		fields[key] = valpart
	}
	return fields
}

func parseCompetitorResponse(body string) (*CompetitorInfo, error) {
	fields := parseProScoreResponse(body)

	if e, ok := fields["E"]; ok {
		return nil, fmt.Errorf("server error: %s", e)
	}

	info := &CompetitorInfo{}
	for k, v := range fields {
		switch k {
		case "Num":          info.Num, _ = strconv.Atoi(v)
		case "FName":        info.FirstName = v
		case "LName":        info.LastName = v
		case "Gym":          info.Gym = v
		case "Level":        info.Level = v
		case "Session":      info.Session = v
		case "Ave_Score1":   info.Score1 = nullableFloat(v)
		case "Start_Value1": info.StartValue1 = nullableFloat(v)
		case "EScore1":      info.EScore1 = nullableFloat(v)
		case "Adjust1":      info.Adjust1 = nullableFloat(v)
		case "Ave_Score2":   info.Score2 = nullableFloat(v)
		case "Start_Value2": info.StartValue2 = nullableFloat(v)
		case "EScore2":      info.EScore2 = nullableFloat(v)
		case "Adjust2":      info.Adjust2 = nullableFloat(v)
		}
	}
	return info, nil
}
func GetCompetitorInfoByHTTP(num, group int) (*CompetitorInfo, error) {
	body := fmt.Sprintf(
		`FC=getcompnum;RE=5;ID:S=2"01";Batt:S=4"100%%";Version:S=6"5.0.13";Num:I=%d;Group:I=%d;`,
		num, group,
	)

	url := fmt.Sprintf("http://127.0.0.1:51514/proscore", host)
	resp, err := http.Post(url, "text/plain", strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return parseCompetitorResponse(string(raw))
}