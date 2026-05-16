package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

func StreamHandler(registry *SessionRegistry) http.Handler {
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

		sessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
		if sessionID == "" {
			sessionID = strings.TrimSpace(r.URL.Query().Get("session_id"))
		}
		if sessionID == "" {
			http.Error(w, "Mcp-Session-Id header or session_id query param required", http.StatusBadRequest)
			return
		}

		session := registry.Get(sessionID)
		if session == nil {
			http.Error(w, "invalid or expired session", http.StatusUnauthorized)
			return
		}

		p := auth.PrincipalFrom(r.Context())
		if session.Principal != nil && p != nil {
			if session.Principal.Kind != p.Kind {
				http.Error(w, "principal mismatch", http.StatusForbidden)
				return
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		fl.Flush()

		var lastSeq int64
		if leid := r.Header.Get("Last-Event-ID"); leid != "" {
			lastSeq, _ = strconv.ParseInt(strings.TrimSpace(leid), 10, 64)
		}

		if lastSeq > 0 {
			for _, ev := range session.EventsSince(lastSeq) {
				writeStreamEvent(w, fl, ev)
			}
		}

		writeStreamEvent(w, fl, SessionEvent{
			Type: "connected",
			Data: map[string]any{"session_id": sessionID},
		})

		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()

		ch := session.EventCh()
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
				writeStreamEvent(w, fl, ev)
			}
		}
	})
}

func writeStreamEvent(w http.ResponseWriter, fl http.Flusher, ev SessionEvent) {
	var b []byte
	if len(ev.RawJSON) > 0 {
		b = ev.RawJSON
	} else {
		data := make(map[string]any)
		for k, v := range ev.Data {
			data[k] = v
		}
		if ev.ExecID != "" {
			data["execution_id"] = ev.ExecID
		}
		if ev.SandboxID != (uuid.UUID{}) {
			data["sandbox_id"] = ev.SandboxID.String()
		}
		b, _ = json.Marshal(data)
	}
	if ev.SeqID > 0 {
		fmt.Fprintf(w, "id: %d\n", ev.SeqID)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
	fl.Flush()
}
