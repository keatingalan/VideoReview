//go:build windows
// +build windows

package main

import (
	"fmt"
	"net"
	"strings"
	"time"

	"videoreview/shared"
)

const (
	portKeypad   = 51520
	portIPad     = 51521
	portScoreGen = 23467
)

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
	appendLog("Waiting for Video Server configuration…")
	httpEndpoint = guiGetHTTPEndpoint()
	updateURLDisplay()
	appendLog("Video Server set: " + httpEndpoint)

	if err := initLocalIPs(); err != nil {
		appendLog("Warning: could not enumerate local IPs: " + err.Error())
	} else {
		appendLog(fmt.Sprintf("Accepting from local IPs: %v", localIPs))
	}

	ports := []int{portKeypad, portIPad, portScoreGen}
	conns := make([]*net.UDPConn, len(ports))
	for i, port := range ports {
		conn, err := bindUDP(port)
		if err != nil {
			for j := 0; j < i; j++ {
				conns[j].Close()
			}
			errMsg := fmt.Sprintf("Could not bind port %d:\n%v", port, err)
			appendLog("ERROR: " + errMsg)
			guiAlert("Fatal Error", errMsg)
			mainWindow.Synchronize(func() {
				lblStatusVal.SetText(fmt.Sprintf("⚠  Failed to bind port %d", port))
			})
			return
		}
		conns[i] = conn
		appendLog(fmt.Sprintf("Listening on 0.0.0.0:%d", port))
	}

	stats.mu.Lock()
	stats.state = stateListening
	stats.mu.Unlock()

	go monitorFailures()

	// Refresh local IPs periodically in case of DHCP changes.
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

	for _, conn := range conns {
		c := conn
		defer c.Close()
		go listenUDP(c)
	}

	select {}
}

func guiGetHTTPEndpoint() string {
	if guiYesNoTimeout("Video Server Setup", "Search for a Video Server via mDNS?\n\nYes = Search mDNS\nNo = Enter URL manually", 10*time.Second, true) {
		appendLog("Searching mDNS (5s)…")
		if ep := searchMDNS(); ep != "" {
			return ep
		}
		appendLog("No mDNS service found — please enter URL manually.")
	}
	for {
		url := guiInputBox("", "Enter Video Server URL:", "http://")
		if url == "" {
			if guiYesNo("No Video Server", "No URL entered. Try mDNS search instead?") {
				if ep := searchMDNS(); ep != "" {
					return ep
				}
			}
			continue
		}
		if !strings.HasPrefix(url, "http://") {
			guiAlert("Invalid URL", "URL must start with http://")
			continue
		}
		return url
	}
}

// listenUDP reads from a bound UDP connection and forwards parsed messages to
// the video server. Only packets from local IPs are processed — ProScore runs
// on the same machine and the listener is not intended to receive network traffic
// (that is the video server's job when run with -listen).
func listenUDP(conn *net.UDPConn) {
	conn.SetReadBuffer(65536)
	destPort := conn.LocalAddr().(*net.UDPAddr).Port
	buffer := make([]byte, 4096)
	oob := make([]byte, 1024)

	for {
		n, _, _, src, err := conn.ReadMsgUDP(buffer, oob)
		if err != nil {
			continue
		}
		data := string(buffer[:n])

		// Port 23467 carries only SCOREGEN-LAST messages; discard the rest.
		if destPort == portScoreGen && (len(data) < 13 || data[:13] != "SCOREGEN-LAST") {
			continue
		}

		appendLog(fmt.Sprintf("Captured %s:%d → port %d - %s", src.IP, src.Port, destPort, data))

		stats.mu.Lock()
		stats.messagesRx++
		stats.queueSize++
		stats.mu.Unlock()

		var msg shared.ProScoreMessage
		var parseErr error
		switch destPort {
		case portIPad:
			msg, parseErr = shared.ParseXMLMessage(src.IP.String(), data, shared.EnrichFromProScore)
		default:
			msg, parseErr = shared.ParseCSVMessage(src.IP.String(), data)
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
