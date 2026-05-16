package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

// NotificationsHandler serves SSE billing events for a sandbox.
// The agent opens this as a second connection alongside POST /v1/mcp for tool calls.
func NotificationsHandler(svc *Service, bus *SandboxEventBus) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		rawID := strings.TrimSpace(r.URL.Query().Get("sandbox_id"))
		sandboxID, err := uuid.Parse(rawID)
		if err != nil || rawID == "" {
			http.Error(w, "sandbox_id query param required", http.StatusBadRequest)
			return
		}

		p := auth.PrincipalFrom(r.Context())
		sb, st, slug, _ := svc.getSandboxForPrincipal(r.Context(), p, sandboxID, map[string]any{
			"sandbox_token": auth.BearerFromRequest(r),
		})
		if sb == nil {
			if st == http.StatusForbidden {
				http.Error(w, slug, http.StatusForbidden)
			} else if st == http.StatusNotFound {
				http.Error(w, slug, http.StatusNotFound)
			} else {
				http.Error(w, "error", http.StatusInternalServerError)
			}
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		fl.Flush()

		ch := bus.Subscribe(sandboxID, 16)
		defer bus.Unsubscribe(sandboxID, ch)

		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()

		// Send initial connected event.
		writeSSE(w, fl, "connected", map[string]any{
			"sandbox_id": sandboxID.String(),
			"status":     sb.Status,
		})

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				fmt.Fprintf(w, ": ping\n\n")
				fl.Flush()
			case ev, ok := <-ch:
				if !ok {
					return
				}
				writeSSE(w, fl, ev.Type, ev.Data)
			}
		}
	})
}

func writeSSE(w http.ResponseWriter, fl http.Flusher, event string, data map[string]any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	fl.Flush()
}
