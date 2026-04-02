package main

import (
	"crypto/tls"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"videoreview/shared"

	"github.com/grandcat/zeroconf"
	"github.com/mdp/qrterminal/v3"
)

// Re-export shared types so the rest of the package can use short names.
type ProScoreMessage = shared.ProScoreMessage
type EventMsg = shared.EventMsg
type VideoFile = shared.VideoFile

const (
	webPort       = 3000 // HTTPS — camera page (requires getUserMedia)
	httpPort      = 3001 // HTTP  — overview and all API endpoints
	keypadUDPPort = 51520
	ipadUDPPort   = 51521
	scoregen1Port = 23467
	uploadDir     = "EventData/videos"
	dbPath        = "EventData/events.db"
	certFile      = "cert.crt"
	keyFile       = "key.pem"
)

//go:embed static
var staticFiles embed.FS

func main() {
	listen := flag.Bool("listen", false, "Enable UDP listeners for keypad and iPad devices")
	flag.Parse()

	os.MkdirAll(uploadDir, 0755)
	initDB()
	defer db.Close()

	if *listen {
		listenUDP()
		log.Printf("UDP listeners active on ports %d, %d, %d", keypadUDPPort, ipadUDPPort, scoregen1Port)
	} else {
		log.Println("UDP listening disabled (pass -listen to enable)")
	}

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create static sub-FS: %v", err)
	}
	staticHandler := http.FileServer(http.FS(staticFS))

	registerShared := func(mux *http.ServeMux) {
		mux.HandleFunc("/ws", wsHandler)
		mux.HandleFunc("/ip", handleIP)
		mux.HandleFunc("/events", handleEvents)
		mux.HandleFunc("/videolist", handleVideoList)
		mux.HandleFunc("/video_list", handleVideoList)
		mux.HandleFunc("/cameralist", handleCameraList)
		mux.HandleFunc("/uploadChunked", handleUploadChunked)
		mux.HandleFunc("/video/", handleVideoServe)
		mux.HandleFunc("/eventlist", handleEventList)
		mux.HandleFunc("/scorelist", handleScoreList)
		mux.HandleFunc("/cameraQR", func(w http.ResponseWriter, r *http.Request) {
			handleQR(w, r, "/", "https")
		})
		mux.HandleFunc("/overviewQR", func(w http.ResponseWriter, r *http.Request) {
			handleQR(w, r, "/overview", "http")
		})
	}

	// HTTPS mux: serves camera.html (requires getUserMedia → secure context)
	// plus all static assets so the camera page can load them.
	httpsMux := http.NewServeMux()
	registerShared(httpsMux)
	httpsMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/camera":
			r.URL.Path = "/camera.html"
		case "/overview":
			r.URL.Path = "/overview.html"
		}
		staticHandler.ServeHTTP(w, r)
	})

	// HTTP mux: overview and API — no cert warning needed.
	httpMux := http.NewServeMux()
	registerShared(httpMux)
	httpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/overview" {
			r.URL.Path = "/overview.html"
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

	mDNSServer, _ := zeroconf.Register("WAG-Video-Review", "_http._tcp", "local.", httpPort, nil, nil)
	defer mDNSServer.Shutdown()

	if len(keys) == 1 {
		ip := addrs[keys[0]][0]
		cameraAddr := fmt.Sprintf("https://%s:%d/", ip, webPort)
		overviewAddr := fmt.Sprintf("http://%s:%d/overview", ip, httpPort)
		log.Printf("Camera  (HTTPS): %s", cameraAddr)
		log.Printf("Overview (HTTP): %s", overviewAddr)
		log.Printf("\nConnect viewer devices to %s\n", overviewAddr)
		qrterminal.GenerateHalfBlock(overviewAddr, qrterminal.L, os.Stdout)
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
