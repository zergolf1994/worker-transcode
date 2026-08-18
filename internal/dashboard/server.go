package dashboard

import (
	"context"
	_ "embed"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

var numberedWorker = regexp.MustCompile(`@\d+$`)

// ShouldStart makes one process own the host dashboard. systemd workers use
// ...@1, ...@2, so @1 is the deterministic owner. A manually started worker
// without a numeric suffix is treated as a single worker and also owns it.
func ShouldStart(workerID string) bool {
	return !numberedWorker.MatchString(workerID) || strings.HasSuffix(workerID, "@1")
}

func workerGroup(workerID string) string {
	if numberedWorker.MatchString(workerID) {
		return numberedWorker.ReplaceAllString(workerID, "")
	}
	return workerID
}

// Start serves the realtime dashboard and shuts down with the worker.
func Start(ctx context.Context, port, workerID, storagePath string) {
	group := workerGroup(workerID)
	hub := newHub(group, storagePath)
	go hub.run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/events", hub.serveEvents)
	mux.HandleFunc("/logs/", serveProcessLog(group))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"worker-transcode-dashboard"}`))
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("📺 Transcode monitor: http://0.0.0.0:%s (owner=%s)", port, workerID)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("⚠️ Dashboard disabled: %v", err)
	}
}
