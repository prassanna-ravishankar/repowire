package hub

import (
	"net/http"
	"os"
	"syscall"
	"time"
)

func (h *Hub) registerShutdownRoute(mux *http.ServeMux) {
	mux.HandleFunc("POST /shutdown", localhostOnly(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "shutting_down"})
		time.AfterFunc(500*time.Millisecond, func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) })
	}))
}
