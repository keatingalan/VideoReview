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
		// Queue message for retry instead of dropping it
		queueForRetry(msg)
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
		// Queue message for retry instead of dropping it
		queueForRetry(msg)
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

// queueForRetry adds a failed message to the retry queue for later retry attempts.
func queueForRetry(msg ProScoreMessage) {
	retryQueue.mu.Lock()
	defer retryQueue.mu.Unlock()
	retryQueue.items = append(retryQueue.items, RetryItem{
		Message:     msg,
		RetryCount:  0,
		LastAttempt: time.Now(),
	})
	appendLog(fmt.Sprintf("Message queued for retry (queue size: %d)", len(retryQueue.items)))
}

// sendToHTTPRetry attempts to send a message and returns whether it succeeded.
// Used by the retry processor to avoid creating duplicate queue entries.
func sendToHTTPRetry(msg ProScoreMessage, item *RetryItem) bool {
	jsonData, err := json.Marshal(msg)
	if err != nil {
		appendLog(fmt.Sprintf("JSON marshal error on retry: %v", err))
		return false
	}

	endpointMu.RLock()
	endpoint := httpEndpoint + "/events"
	endpointMu.RUnlock()

	resp, err := httpClient.Post(endpoint, "application/json", bytes.NewBuffer(jsonData))

	if err != nil {
		appendLog(fmt.Sprintf("Send error on retry (attempt %d): %v", item.RetryCount, err))
		return false
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
		appendLog(fmt.Sprintf("Retry successful (attempt %d)", item.RetryCount))
		return true
	}

	appendLog(fmt.Sprintf("HTTP %d on retry (attempt %d)", resp.StatusCode, item.RetryCount))
	return false
}

// processRetryQueue periodically retries failed messages with exponential backoff.
// Max retries: 5 attempts
// Backoff: 1s, 2s, 4s, 8s, 16s (exponential)
func processRetryQueue() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		retryQueue.mu.Lock()
		if len(retryQueue.items) == 0 {
			retryQueue.mu.Unlock()
			continue
		}

		// Check each item to see if it should be retried
		var itemsToKeep []RetryItem
		for _, item := range retryQueue.items {
			// Calculate backoff time: 1s * 2^retryCount
			backoffDuration := time.Duration(1<<uint(item.RetryCount)) * time.Second
			timeSinceLastAttempt := time.Since(item.LastAttempt)

			if timeSinceLastAttempt >= backoffDuration {
				if item.RetryCount >= 5 {
					// Max retries reached, give up
					appendLog(fmt.Sprintf("Message retried 5 times, giving up: %s", item.Message.FullMessage))
					continue // don't keep this item
				}

				// Time to retry this message
				item.RetryCount++
				item.LastAttempt = time.Now()
				retryQueue.mu.Unlock()

				appendLog(fmt.Sprintf("Retrying message (attempt %d): %s", item.RetryCount, truncateMessage(item.Message.FullMessage.(string), 50)))
				success := sendToHTTPRetry(item.Message, &item)

				retryQueue.mu.Lock()
				if !success {
					// Retry failed, keep item in queue for next attempt
					itemsToKeep = append(itemsToKeep, item)
				}
				// If success is true, we don't add it back (message sent successfully)
				continue
			}

			// Item is waiting for backoff, keep it
			itemsToKeep = append(itemsToKeep, item)
		}
		retryQueue.items = itemsToKeep
		retryQueue.mu.Unlock()
	}
}

func truncateMessage(msg string, maxLen int) string {
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen] + "…"
}
