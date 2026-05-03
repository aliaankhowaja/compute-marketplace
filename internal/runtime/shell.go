package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// ClientFrame is a websocket message from the SDK.
type ClientFrame struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// ServerFrame is emitted by the runtime shell handler.
type ServerFrame struct {
	Type                  string `json:"type"`
	Data                  string `json:"data,omitempty"`
	Balance               int64  `json:"balance,omitempty"`
	Code                  int    `json:"code,omitempty"`
	Message               string `json:"message,omitempty"`
	Bolt11                string `json:"bolt11,omitempty"`
	PaymentHash           string `json:"payment_hash,omitempty"`
	AmountMsats           int64  `json:"amount_msats,omitempty"`
	SuggestedDepositMsats int64  `json:"suggested_deposit_msats,omitempty"`
	ExpiresAt             string `json:"expires_at,omitempty"`
}

var shellUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// ShellHandler upgrades to websocket and attaches an interactive PTY as SANDBOX_USER.
func ShellHandler(cfg *Config, meter *Meter, cb *Callbacks) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := strings.TrimSpace(r.URL.Query().Get("token"))
		if token == "" {
			token = bearerToken(r.Header.Get("Authorization"))
		}

		if _, err := VerifyToken(token, cfg, ScopeShell); err != nil {
			writeAuthError(w, err)
			return
		}

		conn, err := shellUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("runtime/shell: upgrade failed: %v", err)
			return
		}

		sub := meter.Subscribe(32)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer meter.Unsubscribe(sub)
		defer conn.Close()

		cred, err := credForUser(cfg.SandboxUser)
		if err != nil {
			_ = conn.WriteJSON(ServerFrame{Type: "error", Message: "sandbox user lookup failed", Code: http.StatusInternalServerError})
			return
		}

		cmd := exec.CommandContext(ctx, "/bin/bash", "-l")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Dir = "/"

		cred.Setsid = true
		cmd.SysProcAttr = cred

		ptmx, err := pty.Start(cmd)
		if err != nil {
			_ = conn.WriteJSON(ServerFrame{Type: "error", Message: err.Error(), Code: http.StatusInternalServerError})
			return
		}
		defer func() { _ = ptmx.Close() }()

		var wsMu sync.Mutex
		writeFrame := func(sf ServerFrame) {
			wsMu.Lock()
			defer wsMu.Unlock()
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = conn.WriteJSON(sf)
		}

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-sub:
					if !ok {
						return
					}
					switch ev.Type {
					case "billing":
						writeFrame(ServerFrame{Type: "billing", Balance: ev.Balance})
					case "topup":
						writeFrame(ServerFrame{Type: "topup", Balance: ev.Balance, Message: ev.Message})
					case "low_balance":
						writeFrame(ServerFrame{Type: "low_balance", Balance: ev.Balance, Message: ev.Message, SuggestedDepositMsats: ev.SuggestedDepositMsats})
					case "deposit_required":
						writeFrame(ServerFrame{Type: "deposit_required", Balance: ev.Balance, Message: ev.Message, SuggestedDepositMsats: ev.SuggestedDepositMsats})
					case "out_of_balance":
						writeFrame(ServerFrame{Type: "out_of_balance", Balance: ev.Balance})
					case "error":
						writeFrame(ServerFrame{Type: "error", Message: ev.Message, Balance: ev.Balance})
					default:
						writeFrame(ServerFrame{Type: ev.Type, Balance: ev.Balance, Message: ev.Message})
					}
				}
			}
		}()

		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, rerr := ptmx.Read(buf)
				if n > 0 {
					writeFrame(ServerFrame{Type: "stdout", Data: string(buf[:n])})
				}
				if rerr != nil {
					if !errors.Is(rerr, io.EOF) {
						writeFrame(ServerFrame{Type: "stderr", Message: rerr.Error()})
					}
					break
				}
			}
			cancel()
		}()

	readLoop:
		for {
			_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
			_, payload, err := conn.ReadMessage()
			if err != nil {
				break
			}

			var cf ClientFrame
			if err := json.Unmarshal(payload, &cf); err != nil {
				writeFrame(ServerFrame{Type: "error", Message: "invalid json frame"})
				continue
			}

			switch cf.Type {
			case "ping":
				writeFrame(ServerFrame{Type: "billing", Balance: meter.Snapshot().Balance})
			case "resize":
				if cf.Cols > 0 && cf.Rows > 0 {
					_ = pty.Setsize(ptmx, &pty.Winsize{Rows: cf.Rows, Cols: cf.Cols})
				}
			case "request_topup":
				go func(bal0 int64, cons0 int64) {
					ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel2()
					switch strings.ToLower(strings.TrimSpace(cfg.BillingMethod)) {
					case "lightning":
						doc, err := cb.PostRuntimeDepositMint(ctx2, bal0, cons0)
						if err != nil {
							writeFrame(ServerFrame{Type: "error", Message: err.Error()})
							return
						}
						writeFrame(ServerFrame{
							Type:        "deposit_required",
							Balance:     bal0,
							Bolt11:      doc.Bolt11,
							PaymentHash: doc.PaymentHash,
							AmountMsats: doc.AmountMsats,
							ExpiresAt:   doc.ExpiresAt,
							Message:     "pay_bolt11_to_extend_balance",
						})
					default:
						_ = cb.PostCreditAllocateApply(ctx2, bal0, cons0, func(delta int64, newBal int64) {
							if newBal != 0 {
								meter.ApplyNewBalance(newBal)
							} else if delta != 0 {
								meter.ApplyBalanceDelta(delta)
							} else {
								meter.ClearTopupFlight()
							}
						})
					}
				}(meter.Snapshot().Balance, meter.Snapshot().ConsumedTotal)
			case "input", "":
				if cf.Type == "" {
					cf.Type = "input"
				}
				if _, err := io.WriteString(ptmx, cf.Data); err != nil {
					writeFrame(ServerFrame{Type: "error", Message: err.Error()})
					break readLoop
				}
			default:
				writeFrame(ServerFrame{Type: "error", Message: "unknown frame type"})
			}
		}

		cancel()

		waitErr := cmd.Wait()
		code := 0
		if waitErr != nil {
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		writeFrame(ServerFrame{Type: "exit", Code: code})
	})
}
