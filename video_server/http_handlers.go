package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/skip2/go-qrcode"
)

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
		files = append(files, VideoFile{
			CameraDesc: parts[0],
			Length:     length,
			EndTime:    endTime,
			StartTime:  endTime - length,
			Filename:   name,
		})
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
	filename := filepath.Base(r.URL.Query().Get("name"))
	f, err := os.OpenFile(filepath.Join(uploadDir, filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
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

func handleQR(w http.ResponseWriter, r *http.Request, urlPath, scheme string) {
	ip := firstIP()
	port := webPort
	if scheme == "http" {
		port = httpPort
	}
	target := scheme + "://" + ip + ":" + strconv.Itoa(port) + urlPath
	png, err := qrcode.Encode(target, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("data:image/png;base64," + encodeBase64(png)))
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
	var body ProScoreMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("handleEvents decode error: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saveEvent(body)
	hub.broadcast(body)
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
