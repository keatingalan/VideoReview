//go:build windows
// +build windows

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"videoreview/shared"
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
	Timeout: 10 * time.Second,
}

func sendToHTTP(msg shared.ProScoreMessage) {
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
		stats.lastSendOK = time.Now()
		if stats.state == stateNoServer {
			stats.state = stateListening
		}
		stats.mu.Unlock()
	} else {
		stats.mu.Lock()
		stats.messagesFailed++
		stats.consecutiveFails++
		stats.mu.Unlock()
		appendLog(fmt.Sprintf("HTTP %d from endpoint", resp.StatusCode))
	}
}

func monitorFailures() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		stats.mu.Lock()
		fails := stats.consecutiveFails
		stats.mu.Unlock()

		if fails >= 10 {
			stats.mu.Lock()
			stats.state = stateNoServer
			stats.mu.Unlock()

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

	appendLog("⚠  10+ consecutive failures — prompting for new Video Server")

	endpointMu.RLock()
	current := httpEndpoint
	endpointMu.RUnlock()

	if !guiYesNo("Send Failures",
		fmt.Sprintf("10+ consecutive send failures.\nCurrent Video Server: %s\n\nChange Video Server now?", current)) {
		stats.mu.Lock()
		stats.consecutiveFails = 0
		stats.mu.Unlock()
		appendLog("Keeping current Video Server.")
		return
	}

	var newEndpoint string
	if guiYesNo("Change Video Server", "Search via mDNS?\n\nYes = mDNS\nNo = Enter manually") {
		newEndpoint = searchMDNS()
	}
	if newEndpoint == "" {
		newEndpoint = guiInputBox("New Video Server", "Enter Video Server URL:", current)
	}

	if newEndpoint == "" || !strings.HasPrefix(newEndpoint, "http://") {
		appendLog("Invalid/empty Video Server — keeping current.")
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
	appendLog("Video Server updated: " + newEndpoint)
}
