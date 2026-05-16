//go:generate goversioninfo -64 -icon=app.ico -manifest=rsrc.manifest -o=rsrc.syso versioninfo.json
package main

import (
	"compress/gzip"
	"crypto/tls"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
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

type gzipResponseWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.writer.Write(b)
}

func (w *gzipResponseWriter) WriteHeader(statusCode int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(statusCode)
}

func gzipJSONHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		h.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, writer: gz}, r)
	})
}

func gzipFontHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || !isFontAsset(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		h.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, writer: gz}, r)
	})
}

func cacheControlHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isCacheableAsset(r.URL.Path) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		h.ServeHTTP(w, r)
	})
}

func isFontAsset(urlPath string) bool {
	ext := strings.ToLower(path.Ext(urlPath))
	switch ext {
	case ".woff", ".woff2", ".ttf", ".otf", ".svg", ".eot":
		return true
	default:
		return false
	}
}

func isCacheableAsset(urlPath string) bool {
	ext := strings.ToLower(path.Ext(urlPath))
	switch ext {
	case ".js", ".css", ".woff", ".woff2", ".ttf", ".otf", ".svg", ".eot":
		return true
	default:
		return false
	}
}

func main() {
	listen := flag.Bool("listen", false, "Enable UDP listeners for keypad and iPad devices")
	knownIPsFlag := flag.String("scoregen-ips", "", "Comma-separated list of allowed source IPs for UDP messages (e.g. 192.168.1.10,192.168.1.11); empty means allow all")
	flag.Parse()

	var knownIPs map[string]struct{}
	if *knownIPsFlag != "" {
		knownIPs = make(map[string]struct{})
		for _, ip := range strings.Split(*knownIPsFlag, ",") {
			if trimmed := strings.TrimSpace(ip); trimmed != "" {
				knownIPs[trimmed] = struct{}{}
			}
		}
		log.Printf("Restricting UDP sources to: %s", *knownIPsFlag)
	}

	os.MkdirAll(uploadDir, 0755)
	initDB()
	defer db.Close()

	if *listen {
		listenUDP(knownIPs)
		log.Printf("UDP listeners active on ports %d, %d, %d", keypadUDPPort, ipadUDPPort, scoregen1Port)
	} else {
		log.Println("UDP listening disabled (pass -listen to enable)")
	}

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create static sub-FS: %v", err)
	}
	staticHandler := gzipFontHandler(cacheControlHandler(http.FileServer(http.FS(staticFS))))

	registerShared := func(mux *http.ServeMux) {
		mux.HandleFunc("/ws", wsHandler)
		mux.HandleFunc("/ip", handleIP)
		mux.HandleFunc("/events", handleEvents)
		mux.Handle("/videolist", gzipJSONHandler(http.HandlerFunc(handleVideoList)))
		mux.Handle("/video_list", gzipJSONHandler(http.HandlerFunc(handleVideoList)))
		mux.Handle("/cameralist", gzipJSONHandler(http.HandlerFunc(handleCameraList)))
		mux.HandleFunc("/uploadChunked", handleUploadChunked)
		mux.HandleFunc("/video/", handleVideoServe)
		mux.Handle("/eventlist", gzipJSONHandler(http.HandlerFunc(handleEventList)))
		mux.Handle("/scorelist", gzipJSONHandler(http.HandlerFunc(handleScoreList)))
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
