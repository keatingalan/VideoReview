//go:build windows
// +build windows

package main

import (
	"fmt"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// ── Display refresh ───────────────────────────────────────────────────────────

func refreshUI() {
	stats.mu.Lock()
	rx := stats.messagesRx
	sent := stats.messagesSent
	failed := stats.messagesFailed
	queue := stats.queueSize
	uptime := time.Since(stats.startTime)
	cf := stats.consecutiveFails
	state := stats.state
	lastOK := stats.lastSendOK
	stats.mu.Unlock()

	rate := "N/A"
	if rx > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(sent)/float64(rx)*100)
	}

	var status string
	switch state {
	case stateStarting:
		status = "⏳  Starting…"
	case stateNoServer:
		if lastOK.IsZero() {
			status = "⚠  Video Server unreachable"
		} else {
			status = fmt.Sprintf("⚠  Video Server unreachable (last OK %s ago)",
				formatDuration(time.Since(lastOK).Round(time.Second)))
		}
	default: // stateListening
		if cf > 5 {
			status = fmt.Sprintf("⚠  %d consecutive send failures", cf)
		} else if queue > 10 {
			status = fmt.Sprintf("⚠  Queue building: %d", queue)
		} else {
			status = "✓  Listening"
		}
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

// ── Button handlers ───────────────────────────────────────────────────────────

func doChangeEndpoint() {
	endpointMu.RLock()
	cur := httpEndpoint
	endpointMu.RUnlock()

	url := guiInputBox("Change Video Server", "Enter new Video Server URL:", cur)
	if url == "" {
		return
	}
	if len(url) < 7 || url[:7] != "http://" {
		guiAlert("Invalid URL", "URL must start with http://")
		return
	}
	endpointMu.Lock()
	httpEndpoint = url
	endpointMu.Unlock()
	stats.mu.Lock()
	stats.consecutiveFails = 0
	stats.mu.Unlock()
	updateURLDisplay()
	appendLog("Video Server updated: " + url)
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
	appendLog("Video Server set via mDNS: " + ep)
}

// ── Walk dialog helpers ───────────────────────────────────────────────────────

// timerProc is a Windows timer callback. It receives hwnd of the window the
// timer was set on, and we use it to post WM_CLOSE to close the dialog.
// The callback signature is required by SetTimer: it runs on the UI thread
// inside the message loop, so it is safe to call Win32 UI functions directly.
var timerProc = windows.NewCallback(func(hwnd win.HWND, msg uint32, idEvent uintptr, dwTime uint32) uintptr {
	win.KillTimer(hwnd, idEvent)
	win.PostMessage(hwnd, win.WM_CLOSE, 0, 0)
	return 0
})

// guiInputBox shows an input dialog with no timeout.
func guiInputBox(title, prompt, defaultVal string) string {
	return guiInputBoxTimeout(title, prompt, defaultVal, 0)
}

// guiInputBoxTimeout shows an input dialog. If timeout > 0 and the user does
// not respond, defaultVal is returned and the dialog closes automatically.
func guiInputBoxTimeout(title, prompt, defaultVal string, timeout time.Duration) string {
	var dlg *walk.Dialog
	var edit *walk.LineEdit
	var timerLabel *walk.Label
	accepted := false
	resultVal := defaultVal

	minHeight := 140
	if timeout > 0 {
		minHeight = 165
	}

	Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: new(*walk.PushButton),
		CancelButton:  new(*walk.PushButton),
		MinSize:       Size{Width: 420, Height: minHeight},
		Layout:        VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 6},
		Children: []Widget{
			Label{Text: prompt},
			LineEdit{AssignTo: &edit, Text: defaultVal},
			Label{AssignTo: &timerLabel, Text: ""},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					HSpacer{},
					PushButton{
						Text: "OK",
						OnClicked: func() {
							accepted = true
							resultVal = edit.Text()
							dlg.Accept()
						},
					},
					PushButton{
						Text: "Cancel",
						OnClicked: func() {
							accepted = true
							resultVal = ""
							dlg.Cancel()
						},
					},
				},
			},
		},
	}.Create(mainWindow)

	if timeout > 0 {
		hwnd := dlg.Handle()
		const timerID = 1

		// SetTimer fires timerProc every second on the UI thread — no goroutine needed.
		win.SetTimer(hwnd, timerID, 1000, timerProc)

		deadline := time.Now().Add(timeout)
		dlg.Closing().Attach(func(cancelled *bool, reason walk.CloseReason) {
			win.KillTimer(hwnd, timerID)
			if !accepted {
				// Closed by timer (WM_CLOSE) or X button — use default.
				resultVal = defaultVal
			}
		})

		// Update the countdown label each second using a separate ticker.
		// We do this via a goroutine + Synchronize just for the label text —
		// the actual close is handled by timerProc on the UI thread.
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return
				}
				secs := int(remaining.Seconds()) + 1
				dlg.Synchronize(func() {
					if timerLabel != nil {
						timerLabel.SetText(fmt.Sprintf(
							"Auto-selecting \"%s\" in %ds…", defaultVal, secs))
					}
				})
			}
		}()
	}

	dlg.Run()
	return resultVal
}

func guiAlert(title, message string) {
	mainWindow.Synchronize(func() {
		walk.MsgBox(mainWindow, title, message, walk.MsgBoxIconWarning)
	})
}

// guiYesNo shows a yes/no dialog with no timeout.
func guiYesNo(title, message string) bool {
	return guiYesNoTimeout(title, message, 0, false)
}

// guiYesNoTimeout shows a yes/no dialog. If timeout > 0 and the user does not
// respond, defaultVal is returned and the dialog closes automatically.
func guiYesNoTimeout(title, message string, timeout time.Duration, defaultVal bool) bool {
	var dlg *walk.Dialog
	var timerLabel *walk.Label
	answered := false
	resultVal := defaultVal

	defaultText := "No"
	if defaultVal {
		defaultText = "Yes"
	}

	minHeight := 120
	if timeout > 0 {
		minHeight = 145
	}

	Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: new(*walk.PushButton),
		CancelButton:  new(*walk.PushButton),
		MinSize:       Size{Width: 420, Height: minHeight},
		Layout:        VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 6},
		Children: []Widget{
			Label{Text: message},
			Label{AssignTo: &timerLabel, Text: ""},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					HSpacer{},
					PushButton{
						Text: "Yes",
						OnClicked: func() {
							answered = true
							resultVal = true
							dlg.Accept()
						},
					},
					PushButton{
						Text: "No",
						OnClicked: func() {
							answered = true
							resultVal = false
							dlg.Accept()
						},
					},
				},
			},
		},
	}.Create(mainWindow)

	if timeout > 0 {
		hwnd := dlg.Handle()
		const timerID = 2

		win.SetTimer(hwnd, timerID, 1000, timerProc)

		deadline := time.Now().Add(timeout)
		dlg.Closing().Attach(func(cancelled *bool, reason walk.CloseReason) {
			win.KillTimer(hwnd, timerID)
			if !answered {
				resultVal = defaultVal
			}
		})

		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return
				}
				secs := int(remaining.Seconds()) + 1
				dlg.Synchronize(func() {
					if timerLabel != nil {
						timerLabel.SetText(fmt.Sprintf(
							"Auto-selecting \"%s\" in %ds…", defaultText, secs))
					}
				})
			}
		}()
	}

	dlg.Run()
	return resultVal
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

// Suppress unused import warning — unsafe is needed for windows.NewCallback.
var _ = unsafe.Pointer(nil)
