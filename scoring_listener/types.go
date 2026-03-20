//go:build windows
// +build windows

package main

import (
	"sync"
	"time"

	"videoreview/shared"
)

// Re-export shared type so the rest of the package uses a short name.
type ProScoreMessage = shared.ProScoreMessage

type appState int

const (
	stateStarting  appState = iota // waiting for Video Server config / mDNS
	stateListening                 // UDP ports bound, actively forwarding
	stateNoServer                  // consecutive send failures — video server unreachable
)

// Stats tracks message counts and app lifecycle state for the UI display.
type Stats struct {
	mu               sync.Mutex
	messagesRx       int
	messagesSent     int
	messagesFailed   int
	queueSize        int
	startTime        time.Time
	consecutiveFails int
	state            appState
	lastSendOK       time.Time // zero until first successful send
}

var stats = Stats{startTime: time.Now(), state: stateStarting}

var (
	httpEndpoint      string
	endpointMu        sync.RWMutex
	promptingEndpoint bool
	promptingMu       sync.Mutex
)
