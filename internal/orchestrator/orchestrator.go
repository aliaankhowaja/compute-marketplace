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
	baseURL := strings.TrimRight(cfg.RunPodAPIURL, "/")
	apiKey := cfg.RunPodAPIKey
	if cfg.UsePodMock {
		baseURL = strings.TrimRight(cfg.PodMockURL, "/") + "/v1"
		apiKey = "mock"
	}
	return &RunPodClient{
		BaseURL: baseURL,
		APIKey:  apiKey,
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

// CreatePodRequest matches RunPod REST POST /pods (PodCreateInput); fields are additive over time.
// See https://docs.runpod.io/api-reference/pods/POST/pods
type CreatePodRequest struct {
	TemplateID string            `json:"templateId,omitempty"`
	Name       string            `json:"name,omitempty"`
	Env        map[string]string `json:"env,omitempty"`

	ComputeType       string   `json:"computeType,omitempty"` // CPU | GPU
	CPUFlavorIDs      []string `json:"cpuFlavorIds,omitempty"`
	CPUFlavorPriority string   `json:"cpuFlavorPriority,omitempty"`
	VCPUCount         int      `json:"vcpuCount,omitempty"`
	GPUCount          int      `json:"gpuCount,omitempty"`
	MinVCPUPerGPU     int      `json:"minVCPUPerGPU,omitempty"`
	MinRAMPerGPU      int      `json:"minRAMPerGPU,omitempty"`
}

// ResolveTier maps a pricing tier to RunPod CreatePodRequest fields plus labels stored on sandboxes.
func ResolveTier(cfg *config.Control, tier string) (req CreatePodRequest, templateLabel string, runtime string, err error) {
	if cfg == nil {
		return CreatePodRequest{}, "", "", fmt.Errorf("config is nil")
	}
	switch strings.TrimSpace(strings.ToLower(tier)) {
	case "nano":
		id := strings.TrimSpace(cfg.RunPodTemplateNanoID)
		if id == "" {
			id = strings.TrimSpace(cfg.RunPodTemplateCPUID)
		}
		if id == "" {
			return CreatePodRequest{}, "", "", fmt.Errorf("RUNPOD_TEMPLATE_NANO_ID or RUNPOD_TEMPLATE_CPU_ID is required")
		}
		return CreatePodRequest{
			TemplateID:        id,
			ComputeType:       "CPU",
			CPUFlavorIDs:      []string{"cpu3c"},
			CPUFlavorPriority: "availability",
			VCPUCount:         1,
		}, "sockt-nano", "runpod_cpu", nil

	case "micro":
		id := strings.TrimSpace(cfg.RunPodTemplateMicroID)
		if id == "" {
			id = strings.TrimSpace(cfg.RunPodTemplateCPUID)
		}
		if id == "" {
			return CreatePodRequest{}, "", "", fmt.Errorf("RUNPOD_TEMPLATE_MICRO_ID or RUNPOD_TEMPLATE_CPU_ID is required")
		}
		return CreatePodRequest{
			TemplateID:        id,
			ComputeType:       "CPU",
			CPUFlavorIDs:      []string{"cpu3c"},
			CPUFlavorPriority: "availability",
			VCPUCount:         2,
		}, "sockt-micro", "runpod_cpu", nil

	case "standard":
		id := strings.TrimSpace(cfg.RunPodTemplateStandardID)
		if id == "" {
			id = strings.TrimSpace(cfg.RunPodTemplateCPUID)
		}
		if id == "" {
			return CreatePodRequest{}, "", "", fmt.Errorf("RUNPOD_TEMPLATE_STANDARD_ID or RUNPOD_TEMPLATE_CPU_ID is required")
		}
		return CreatePodRequest{
			TemplateID:        id,
			ComputeType:       "CPU",
			CPUFlavorIDs:      []string{"cpu3c"},
			CPUFlavorPriority: "availability",
			VCPUCount:         4,
		}, "sockt-standard", "runpod_cpu", nil

	case "gpu_small":
		id := strings.TrimSpace(cfg.RunPodTemplateGPUSmallID)
		if id == "" {
			return CreatePodRequest{}, "", "", fmt.Errorf("RUNPOD_TEMPLATE_GPU_SMALL_ID is required")
		}
		return CreatePodRequest{
			TemplateID:    id,
			ComputeType:   "GPU",
			GPUCount:      1,
			MinVCPUPerGPU: 6,
			MinRAMPerGPU:  16,
		}, "sockt-gpu-small", "runpod_gpu", nil

	case "gpu_large":
		id := strings.TrimSpace(cfg.RunPodTemplateGPULargeID)
		if id == "" {
			return CreatePodRequest{}, "", "", fmt.Errorf("RUNPOD_TEMPLATE_GPU_LARGE_ID is required")
		}
		return CreatePodRequest{
			TemplateID:    id,
			ComputeType:   "GPU",
			GPUCount:      1,
			MinVCPUPerGPU: 6,
			MinRAMPerGPU:  32,
		}, "sockt-gpu-large", "runpod_gpu", nil

	default:
		return CreatePodRequest{}, "", "", fmt.Errorf("unknown tier %q", tier)
	}
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
	_, code, err := c.do(ctx, http.MethodPost, "/pods/"+id+"/update", body)
	if err != nil {
		return err
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
func (s *Service) CreateSandbox(ctx context.Context, sb *store.Sandbox, env map[string]string) error {
	if s == nil || s.Client == nil || s.Store == nil || s.Cfg == nil {
		return fmt.Errorf("orchestrator not configured")
	}
	partial, _, _, err := ResolveTier(s.Cfg, sb.Tier)
	if err != nil {
		return err
	}
	req := partial
	req.Name = "sockt-" + sb.ID.String()[:8]
	req.Env = env
	pid, err := s.Client.CreatePod(ctx, req)
	if err != nil {
		return err
	}
	env["POD_ID"] = pid
	if err := s.Client.UpdatePodEnv(ctx, pid, env); err != nil {
		return fmt.Errorf("runpod set env: %w", err)
	}
	proxy := RunPodProxyURL(s.Cfg, pid)
	if err := s.Store.UpdateSandboxPod(ctx, sb.ID, strPtr(pid), strPtr(proxy), sb.NetworkVolumeID); err != nil {
		return err
	}
	if err := s.waitForRuntime(ctx, proxy); err != nil {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_ = s.Client.TerminatePod(cleanCtx, pid)
		_ = s.Store.TerminateSandbox(cleanCtx, sb.ID)
		return fmt.Errorf("runtime not ready: %w", err)
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
	proxy := RunPodProxyURL(s.Cfg, podID)
	if err := s.waitForRuntime(ctx, proxy); err != nil {
		return fmt.Errorf("runtime not ready after resume: %w", err)
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

func RunPodProxyURL(cfg *config.Control, podID string) string {
	podID = strings.TrimSpace(podID)
	if podID == "" {
		return ""
	}
	if cfg != nil && cfg.UsePodMock {
		return strings.TrimRight(cfg.PodMockURL, "/") + "/proxy/" + podID
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
