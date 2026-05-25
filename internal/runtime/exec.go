package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxExecOutputBytes = 10 << 20 // 10 MB per stream

type execRequest struct {
	Argv           []string `json:"argv"`
	CWD            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type execResponse struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
}

type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	remaining := lb.limit - lb.buf.Len()
	if remaining <= 0 {
		lb.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		lb.overflow = true
		p = p[:remaining]
	}
	return lb.buf.Write(p)
}

func (lb *limitedBuffer) String() string {
	return lb.buf.String()
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

		var ctx context.Context
		var cancel context.CancelFunc
		if req.TimeoutSeconds > 0 {
			timeout := time.Duration(req.TimeoutSeconds) * time.Second
			ctx, cancel = context.WithTimeout(r.Context(), timeout)
		} else {
			ctx, cancel = context.WithCancel(r.Context())
		}
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
		cmd.SysProcAttr = cred.SysProcAttr
		cmd.Env = sandboxEnv(cred, cfg.SandboxRoot)

		stdout := &limitedBuffer{limit: maxExecOutputBytes}
		stderr := &limitedBuffer{limit: maxExecOutputBytes}
		cmd.Stdout = stdout
		cmd.Stderr = stderr

		runErr := cmd.Run()
		exitCode := 0
		if runErr != nil {
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				exitCode = ee.ExitCode()
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "command execution failed"})
				return
			}
		}

		writeJSON(w, http.StatusOK, execResponse{
			ExitCode:  exitCode,
			Stdout:    stdout.String(),
			Stderr:    stderr.String(),
			Truncated: stdout.overflow || stderr.overflow,
		})
	})
}

type sandboxCred struct {
	SysProcAttr *syscall.SysProcAttr
	HomeDir     string
	Username    string
}

func credForUser(username string) (*sandboxCred, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, err
	}
	uid64, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	if uid64 == 0 {
		return nil, fmt.Errorf("refusing to execute as root (uid 0)")
	}
	gid64, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	return &sandboxCred{
		SysProcAttr: &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: uint32(uid64),
				Gid: uint32(gid64),
			},
		},
		HomeDir:  u.HomeDir,
		Username: u.Username,
	}, nil
}

// sandboxEnv builds a clean environment for sandbox subprocesses, avoiding
// leaking the runtime's root HOME or PATH into user commands.
func sandboxEnv(cred *sandboxCred, sandboxRoot string) []string {
	home := cred.HomeDir
	if home == "" {
		home = sandboxRoot
	}
	return []string{
		"HOME=" + home,
		"USER=" + cred.Username,
		"LOGNAME=" + cred.Username,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"SHELL=/bin/bash",
		"LANG=C.UTF-8",
		"PYTHONUNBUFFERED=1",
		"TERM=xterm-256color",
	}
}
