package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type runtimeHealthBody struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

// WaitForDataPlaneHealth polls GET {proxyBaseURL}/healthz until sockt-runtime returns a successful
// health JSON or ctx is cancelled. proxyBaseURL must be an origin such as https://pod-8443.proxy.runpod.net.
func WaitForDataPlaneHealth(ctx context.Context, proxyBaseURL string, httpClient *http.Client, pollInterval time.Duration) error {
	base := strings.TrimRight(strings.TrimSpace(proxyBaseURL), "/")
	if base == "" {
		return fmt.Errorf("empty proxy url")
	}
	if httpClient == nil {
		return fmt.Errorf("nil http client")
	}
	if pollInterval < 50*time.Millisecond {
		pollInterval = 50 * time.Millisecond
	}
	u := base + "/healthz"
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return fmt.Errorf("%w (last error: %v)", err, lastErr)
			}
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			if !sleepPoll(ctx, pollInterval) {
				return ctx.Err()
			}
			continue
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			if !sleepPoll(ctx, pollInterval) {
				if lastErr != nil {
					return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
				}
				return ctx.Err()
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var h runtimeHealthBody
			if json.Unmarshal(body, &h) == nil && (strings.EqualFold(strings.TrimSpace(h.Status), "ok") || strings.TrimSpace(h.Service) == "sockt-runtime") {
				return nil
			}
			lastErr = fmt.Errorf("unexpected healthz body (HTTP %d)", resp.StatusCode)
		} else {
			lastErr = fmt.Errorf("healthz HTTP %d", resp.StatusCode)
		}

		if !sleepPoll(ctx, pollInterval) {
			if lastErr != nil {
				return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
			}
			return ctx.Err()
		}
	}
}

func sleepPoll(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func isGPUTier(tier string) bool {
	t := strings.ToLower(strings.TrimSpace(tier))
	return t == "gpu_small" || t == "gpu_large"
}

func (s *Service) waitForRuntime(ctx context.Context, proxyBase string, tier string) error {
	if s == nil || s.Cfg == nil {
		return fmt.Errorf("orchestrator not configured")
	}
	timeout := s.Cfg.SandboxReadyTimeout
	if isGPUTier(tier) && s.Cfg.SandboxReadyTimeoutGPU > 0 {
		timeout = s.Cfg.SandboxReadyTimeoutGPU
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	poll := s.Cfg.SandboxReadyPollInterval
	if poll <= 0 {
		poll = 750 * time.Millisecond
	}
	probeTO := s.Cfg.SandboxReadyProbeTimeout
	if probeTO <= 0 {
		probeTO = 8 * time.Second
	}
	client := &http.Client{Timeout: probeTO}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return WaitForDataPlaneHealth(waitCtx, proxyBase, client, poll)
}
