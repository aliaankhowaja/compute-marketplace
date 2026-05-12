package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func NewRouter(st *Store, dm *DockerManager, proxyHost string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz())
	mux.HandleFunc("POST /v1/pods", handleCreatePod(st, dm))
	mux.HandleFunc("POST /v1/pods/{id}/stop", handleStopPod(st, dm))
	mux.HandleFunc("POST /v1/pods/{id}/start", handleStartPod(st, dm))
	mux.HandleFunc("POST /v1/pods/{id}/update", handleUpdatePod(st))
	mux.HandleFunc("DELETE /v1/pods/{id}", handleDeletePod(st, dm))
	mux.HandleFunc("/proxy/{id}/", handleProxy(st, proxyHost))
	mux.HandleFunc("/proxy/{id}", handleProxy(st, proxyHost))
	return mux
}

func handleHealthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"service": "pod-mock",
		})
	}
}

func handleCreatePod(st *Store, dm *DockerManager) http.HandlerFunc {
	type createReq struct {
		TemplateID   string            `json:"templateId"`
		Name         string            `json:"name"`
		Env          map[string]string `json:"env"`
		ComputeType  string            `json:"computeType"`
		CPUFlavorIDs []string          `json:"cpuFlavorIds"`
		VCPUCount    int               `json:"vcpuCount"`
		GPUCount     int               `json:"gpuCount"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req createReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}

		containerID, hostPort, err := dm.CreateContainer(r.Context(), req.Name, req.Env)
		if err != nil {
			slog.Error("create_container", "err", err)
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}

		p := st.Create(req.Name, req.Env, containerID, hostPort)
		slog.Info("pod_created", "id", p.ID, "container", containerID[:12], "port", hostPort)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": p.ID})
	}
}

func handleStopPod(st *Store, dm *DockerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		pod, ok := st.Get(id)
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if err := dm.StopContainer(r.Context(), pod.ContainerID); err != nil {
			slog.Error("stop_container", "id", id, "err", err)
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		_ = st.Stop(id)
		slog.Info("pod_stopped", "id", id)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"stopped"}`))
	}
}

func handleStartPod(st *Store, dm *DockerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		pod, ok := st.Get(id)
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if err := dm.StartContainer(r.Context(), pod.ContainerID); err != nil {
			slog.Error("start_container", "id", id, "err", err)
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		_ = st.Start(id)
		slog.Info("pod_started", "id", id)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"running"}`))
	}
}

func handleUpdatePod(st *Store) http.HandlerFunc {
	type updateReq struct {
		Env map[string]string `json:"env"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req updateReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		if err := st.UpdateEnv(id, req.Env); err != nil {
			if strings.Contains(err.Error(), "not found") {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		slog.Info("pod_env_updated", "id", id, "keys", len(req.Env))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"updated"}`))
	}
}

func handleDeletePod(st *Store, dm *DockerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		pod, ok := st.Get(id)
		if ok && pod.ContainerID != "" {
			_ = dm.RemoveContainer(r.Context(), pod.ContainerID)
		}
		_ = st.Terminate(id)
		slog.Info("pod_terminated", "id", id)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"terminated"}`))
	}
}

func handleProxy(st *Store, proxyHost string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		pod, ok := st.Get(id)
		if !ok {
			http.Error(w, `{"error":"pod not found"}`, http.StatusNotFound)
			return
		}
		if pod.State != StateRunning {
			http.Error(w, `{"error":"pod not running"}`, http.StatusServiceUnavailable)
			return
		}

		target := fmt.Sprintf("http://%s:%d", proxyHost, pod.HostPort)
		targetURL, _ := url.Parse(target)
		prefix := "/proxy/" + id

		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = targetURL.Scheme
				req.URL.Host = targetURL.Host
				req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
				if req.URL.Path == "" {
					req.URL.Path = "/"
				}
				req.Host = targetURL.Host
			},
			FlushInterval: -1,
		}
		proxy.ServeHTTP(w, r)
	}
}
