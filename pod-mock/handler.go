package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

func NewRouter(st *Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz())
	mux.HandleFunc("POST /v1/pods", handleCreatePod(st))
	mux.HandleFunc("POST /v1/pods/{id}/stop", handleStopPod(st))
	mux.HandleFunc("POST /v1/pods/{id}/start", handleStartPod(st))
	mux.HandleFunc("POST /v1/pods/{id}/update", handleUpdatePod(st))
	mux.HandleFunc("DELETE /v1/pods/{id}", handleDeletePod(st))
	return mux
}

func handleHealthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Info("healthz", "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"service": "sockt-runtime",
		})
	}
}

func handleCreatePod(st *Store) http.HandlerFunc {
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
		p := st.Create(req.Name, req.Env)
		slog.Info("pod_created", "id", p.ID, "name", req.Name, "compute", req.ComputeType)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": p.ID})
	}
}

func handleStopPod(st *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := st.Stop(id); err != nil {
			if strings.Contains(err.Error(), "not found") {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		slog.Info("pod_stopped", "id", id)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"stopped"}`))
	}
}

func handleStartPod(st *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := st.Start(id); err != nil {
			if strings.Contains(err.Error(), "not found") {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
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

func handleDeletePod(st *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		_ = st.Terminate(id)
		slog.Info("pod_terminated", "id", id)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"terminated"}`))
	}
}
