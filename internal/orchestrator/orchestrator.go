// Package orchestrator drives RunPod lifecycle and sandbox persistence.
package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/config"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// Service coordinates RunPod + store updates.
type Service struct {
	Client *RunPodClient
	Store  *store.Store
	Cfg    *config.Control
}

// RunPodClient is a thin REST client.
type RunPodClient struct {
	BaseURL string // e.g. https://rest.runpod.io/v1
	APIKey  string
	HTTP    *http.Client
}

func NewRunPodClient(cfg *config.Control) *RunPodClient {
	return &RunPodClient{
		BaseURL: strings.TrimRight(cfg.RunPodAPIURL, "/"),
		APIKey:  cfg.RunPodAPIKey,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *RunPodClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.APIKey))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return out, resp.StatusCode, err
}

// CreatePodRequest is a best-effort shape for RunPod REST /pods.
type CreatePodRequest struct {
	TemplateID string            `json:"templateId,omitempty"`
	Name       string            `json:"name,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	// Some deployments expect list form — callers may adapt later.
}

type podCreateResp struct {
	ID string `json:"id"`
}

func (c *RunPodClient) CreatePod(ctx context.Context, req CreatePodRequest) (podID string, err error) {
	raw, code, err := c.do(ctx, http.MethodPost, "/pods", req)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("runpod create: status=%d body=%s", code, strings.TrimSpace(string(raw)))
	}
	var pr podCreateResp
	if err := json.Unmarshal(raw, &pr); err == nil && pr.ID != "" {
		return pr.ID, nil
	}
	var alt map[string]json.RawMessage
	if err := json.Unmarshal(raw, &alt); err == nil {
		if v, ok := alt["podId"]; ok {
			var s string
			_ = json.Unmarshal(v, &s)
			if s != "" {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("runpod create: missing id in %s", string(raw))
}

func (c *RunPodClient) StopPod(ctx context.Context, id string) error {
	_, code, err := c.do(ctx, http.MethodPost, "/pods/"+id+"/stop", map[string]any{})
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		return fmt.Errorf("runpod: pod not found")
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("runpod stop: status=%d", code)
	}
	return nil
}

func (c *RunPodClient) StartPod(ctx context.Context, id string) error {
	_, code, err := c.do(ctx, http.MethodPost, "/pods/"+id+"/start", map[string]any{})
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("runpod start: status=%d", code)
	}
	return nil
}

func (c *RunPodClient) UpdatePodEnv(ctx context.Context, id string, env map[string]string) error {
	if len(env) == 0 {
		return nil
	}
	body := map[string]any{"env": env}
	_, code, err := c.do(ctx, http.MethodPut, "/pods/"+id, body)
	if err != nil {
		return err
	}
	// Some deployments use PATCH; allow 200-299 or 404 (caller may need different path).
	if code == http.StatusNotFound {
		_, code2, err2 := c.do(ctx, http.MethodPatch, "/pods/"+id, body)
		if err2 != nil {
			return err2
		}
		if code2 >= 200 && code2 < 300 {
			return nil
		}
		return fmt.Errorf("runpod patch env: status=%d", code2)
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("runpod put env: status=%d", code)
	}
	return nil
}

func (c *RunPodClient) TerminatePod(ctx context.Context, id string) error {
	_, code, err := c.do(ctx, http.MethodDelete, "/pods/"+id, nil)
	if err != nil {
		return err
	}
	if (code < 200 || code >= 300) && code != http.StatusNotFound {
		return fmt.Errorf("runpod delete: status=%d", code)
	}
	return nil
}

// CreateSandbox boots a pod and records DB metadata.
func (s *Service) CreateSandbox(ctx context.Context, sb *store.Sandbox, template *store.Template, env map[string]string) error {
	if s == nil || s.Client == nil || s.Store == nil {
		return fmt.Errorf("orchestrator not configured")
	}
	req := CreatePodRequest{
		TemplateID: strings.TrimSpace(template.RunpodTemplateID),
		Name:       "sockt-" + sb.ID.String()[:8],
		Env:        env,
	}
	pid, err := s.Client.CreatePod(ctx, req)
	if err != nil {
		return err
	}
	env["POD_ID"] = pid
	if err := s.Client.UpdatePodEnv(ctx, pid, env); err != nil {
		return fmt.Errorf("runpod set env: %w", err)
	}
	proxy := fmt.Sprintf("https://%s-8443.proxy.runpod.net", pid)
	if err := s.Store.UpdateSandboxPod(ctx, sb.ID, strPtr(pid), strPtr(proxy), sb.NetworkVolumeID); err != nil {
		return err
	}
	if err := s.Store.UpdateSandboxStatus(ctx, sb.ID, "running"); err != nil {
		return err
	}
	return nil
}

func strPtr(s string) *string { return &s }

// PauseSandbox stops the RunPod instance.
func (s *Service) PauseSandbox(ctx context.Context, podID string, sandboxID uuid.UUID) error {
	if podID == "" {
		return fmt.Errorf("missing pod id")
	}
	if err := s.Client.StopPod(ctx, podID); err != nil {
		return err
	}
	return s.Store.UpdateSandboxStatus(ctx, sandboxID, "paused")
}

// ResumeSandbox starts a stopped pod.
func (s *Service) ResumeSandbox(ctx context.Context, podID string, sandboxID uuid.UUID) error {
	if err := s.Client.StartPod(ctx, podID); err != nil {
		return err
	}
	return s.Store.UpdateSandboxStatus(ctx, sandboxID, "running")
}

// TerminateSandbox deletes pod and marks terminated locally.
func (s *Service) TerminateSandbox(ctx context.Context, podID string, sandboxID uuid.UUID) error {
	if podID != "" {
		_ = s.Client.TerminatePod(ctx, podID)
	}
	return s.Store.TerminateSandbox(ctx, sandboxID)
}

func RunPodProxyURL(podID string) string {
	podID = strings.TrimSpace(podID)
	if podID == "" {
		return ""
	}
	return fmt.Sprintf("https://%s-8443.proxy.runpod.net", podID)
}

func SandboxDataEndpoint(base string, sandboxID uuid.UUID) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + "/v1/sandbox/" + sandboxID.String()
}

func SandboxShellWSS(proxyHost string, sandboxID uuid.UUID) string {
	// proxyHost like "<pod>-8443.proxy.runpod.net"
	ph := strings.TrimSpace(proxyHost)
	if ph == "" {
		return ""
	}
	ph = strings.TrimPrefix(strings.TrimPrefix(ph, "https://"), "http://")
	return fmt.Sprintf("wss://%s/sandbox/%s/shell", ph, sandboxID.String())
}
