package runtime

import (
	"net/http"
	"runtime/debug"
)

type statusResponse struct {
	Service string           `json:"service"`
	Version string           `json:"version,omitempty"`
	Balance RuntimeBalance   `json:"balance"`
	Commit  string           `json:"commit,omitempty"`
}

// StatusHandler returns meter snapshot + runtime metadata.
func StatusHandler(cfg *Config, meter *Meter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := bearerToken(r.Header.Get("Authorization"))
		if _, err := VerifyToken(token, cfg, ScopeExec); err != nil {
			// Status is useful for exec-capable tokens; keep parity with other routes by requiring auth.
			writeAuthError(w, err)
			return
		}

		info, ok := debug.ReadBuildInfo()
		modVersion := ""
		if ok {
			modVersion = info.Main.Version
		}

		writeJSON(w, http.StatusOK, statusResponse{
			Service: "sockt-runtime",
			Version: modVersion,
			Balance: meter.Snapshot(),
		})
	})
}
