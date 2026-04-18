//go:build windows
// +build windows

//go:generate goversioninfo -64 -icon=app.ico -manifest=rsrc.manifest -o=rsrc.syso versioninfo.json

package main

import (
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// ── GUI widget handles ────────────────────────────────────────────────────────

var (
	mainWindow   *walk.MainWindow
	lblStatusVal *walk.Label
	lblUptimeVal *walk.Label
	lblRxVal     *walk.Label
	lblSentVal   *walk.Label
	lblFailedVal *walk.Label
	lblQueueVal  *walk.Label
	lblRateVal   *walk.Label
	urlEdit      *walk.LineEdit
	logEdit      *walk.TextEdit
)

var startupTimerProc = windows.NewCallback(func(hwnd win.HWND, msg uint32, idEvent uintptr, dwTime uint32) uintptr {
	win.KillTimer(hwnd, idEvent)
	go startApp()
	return 0
})

func main() {
	err := MainWindow{
		AssignTo: &mainWindow,
		Title:    "Capture ProScore ScoreGen Messages",
		MinSize:  Size{Width: 660, Height: 540},
		Size:     Size{Width: 800, Height: 600},
		Layout:   VBox{Margins: Margins{Left: 8, Top: 8, Right: 8, Bottom: 8}, Spacing: 6},
		Children: []Widget{

			GroupBox{
				Title:  "Status",
				Layout: Grid{Columns: 4, Spacing: 10},
				Children: []Widget{
					Label{Text: "Status:"},
					Label{AssignTo: &lblStatusVal, Text: "Starting…", ColumnSpan: 3},

					Label{Text: "Uptime:"},
					Label{AssignTo: &lblUptimeVal, Text: "00:00", ColumnSpan: 3},

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
						Text:    "Change Video Server",
						MaxSize: Size{Width: 130},
						OnClicked: func() {
							doChangeEndpoint()
						},
					},
					PushButton{
						Text:    "Search mDNS",
						MaxSize: Size{Width: 110},
						OnClicked: func() {
							doMDNS()
						},
					},
				},
			},

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

	// Load the embedded icon (resource ID 1) using LoadIconWithScaleDown,
	// the same API walk uses internally. Send WM_SETICON for both slots so
	// it appears in the title bar and taskbar/Alt-Tab.
	hInstance := win.GetModuleHandle(nil)
	res := win.MAKEINTRESOURCE(1)
	var hIconBig win.HICON
	var hIconSmall win.HICON
	win.LoadIconWithScaleDown(hInstance, res, 32, 32, &hIconBig)
	win.LoadIconWithScaleDown(hInstance, res, 16, 16, &hIconSmall)
	if hIconBig != 0 {
		win.SendMessage(mainWindow.Handle(), win.WM_SETICON, 1, uintptr(hIconBig))
	}
	if hIconSmall != 0 {
		win.SendMessage(mainWindow.Handle(), win.WM_SETICON, 0, uintptr(hIconSmall))
	}

	const startupTimerID = 1
	win.SetTimer(mainWindow.Handle(), startupTimerID, 10, startupTimerProc)

	// Refresh stats display every 500ms on the UI goroutine.
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
