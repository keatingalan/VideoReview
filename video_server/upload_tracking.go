package main

import "sync"

var activeUploads = struct {
	sync.Mutex
	files map[string]int
}{files: make(map[string]int)}

func beginActiveUpload(name string) {
	activeUploads.Lock()
	activeUploads.files[name]++
	activeUploads.Unlock()
}

func endActiveUpload(name string) {
	activeUploads.Lock()
	if activeUploads.files[name] <= 1 {
		delete(activeUploads.files, name)
	} else {
		activeUploads.files[name]--
	}
	activeUploads.Unlock()
}

// withActiveUploadsLocked keeps the active-upload set stable while cleanup
// checks and deletes recordings.
func withActiveUploadsLocked(action func(isActive func(string) bool)) {
	activeUploads.Lock()
	defer activeUploads.Unlock()

	action(func(name string) bool {
		return activeUploads.files[name] > 0
	})
}
