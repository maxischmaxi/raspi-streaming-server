package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

var (
	tracksMu    sync.Mutex
	videoTracks []*webrtc.TrackLocalStaticRTP
	onceStart   sync.Once
)

func basicAuth(handler http.HandlerFunc, username, password string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Basic ") {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		payload, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		parts := strings.SplitN(string(payload), ":", 2)
		if len(parts) != 2 || parts[0] != username || parts[1] != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	username := "max"
	password := "wdghkla123"

	http.HandleFunc("/", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	}, username, password))

	http.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		var offer webrtc.SessionDescription
		if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		videoTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264,
		}, "video", "pion")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		_, err = peerConnection.AddTrack(videoTrack)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
			log.Println("ICE state:", state.String())
		})

		tracksMu.Lock()
		videoTracks = append(videoTracks, videoTrack)
		tracksMu.Unlock()

		onceStart.Do(func() {
			go streamFromCamera(ctx)
		})

		if err := peerConnection.SetRemoteDescription(offer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err = peerConnection.SetLocalDescription(answer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		_ = json.NewEncoder(w).Encode(answer)
	})

	server := &http.Server{Addr: ":8080"}
	go func() {
		log.Println("WebRTC Server läuft auf :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP Server Fehler: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Beende Server...")
	_ = server.Close()
}

func streamFromCamera(ctx context.Context) {
	addr := ":5004"
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("Fehler beim Hören auf UDP %s: %v", addr, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Fatalf("error: %v", err)
		}
	}()

	cmd := exec.Command("bash", "-c", `
libcamera-vid --codec yuv420 --width 640 --height 480 --framerate 25 --timeout 0 --nopreview -o - |
ffmpeg -f rawvideo -pixel_format yuv420p -video_size 640x480 -framerate 25 -i - \
-an -c:v libx264 -preset ultrafast -tune zerolatency -f rtp rtp://127.0.0.1:5004
`)
	cmd.Stderr = log.Writer()
	if err := cmd.Start(); err != nil {
		log.Fatalf("Fehler beim Start der Kamera-Pipeline: %v", err)
	}

	go func() {
		<-ctx.Done()
		log.Println("Stoppe Kamera-Pipeline...")
		_ = cmd.Process.Kill()
	}()

	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				log.Printf("Fehler beim Lesen von UDP: %v", err)
				continue
			}

			packet := &rtp.Packet{}
			if err := packet.Unmarshal(buf[:n]); err != nil {
				log.Printf("Fehler beim Unmarshal: %v", err)
				continue
			}

			tracksMu.Lock()
			for _, track := range videoTracks {
				if err := track.WriteRTP(packet); err != nil {
					log.Printf("Fehler beim Schreiben RTP: %v", err)
				}
			}
			tracksMu.Unlock()
		}
	}
}
