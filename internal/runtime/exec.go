package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type execRequest struct {
	Argv           []string `json:"argv"`
	CWD            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// ExecHandler runs a subprocess as SANDBOX_USER (Linux credentials).
func ExecHandler(cfg *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := bearerToken(r.Header.Get("Authorization"))
		if _, err := VerifyToken(token, cfg, ScopeExec); err != nil {
			writeAuthError(w, err)
			return
		}

		var req execRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if len(req.Argv) == 0 {
			http.Error(w, "argv required", http.StatusBadRequest)
			return
		}

		timeout := 120 * time.Second
		if req.TimeoutSeconds > 0 {
			timeout = time.Duration(req.TimeoutSeconds) * time.Second
			if timeout > 30*time.Minute {
				timeout = 30 * time.Minute
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		cred, err := credForUser(cfg.SandboxUser)
		if err != nil {
			http.Error(w, "sandbox user lookup failed", http.StatusInternalServerError)
			return
		}

		workDir := cfg.SandboxRoot
		if cwd := strings.TrimSpace(req.CWD); cwd != "" {
			d, err := resolveSandboxPath(cfg.SandboxRoot, cwd)
			if err != nil {
				http.Error(w, "invalid cwd", http.StatusBadRequest)
				return
			}
			workDir = d
		}

		cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
		cmd.Dir = workDir
		cmd.SysProcAttr = cred

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		runErr := cmd.Run()
		exitCode := 0
		if runErr != nil {
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				exitCode = ee.ExitCode()
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": runErr.Error()})
				return
			}
		}

		writeJSON(w, http.StatusOK, execResponse{
			ExitCode: exitCode,
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
		})
	})
}

func credForUser(username string) (*syscall.SysProcAttr, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, err
	}
	uid64, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid64, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid64),
			Gid: uint32(gid64),
		},
	}, nil
}
