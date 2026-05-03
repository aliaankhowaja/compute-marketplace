package runtime

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type writeFileRequest struct {
	Path           string `json:"path"`
	Mode           uint32 `json:"mode"`
	ContentBase64  string `json:"content_base64"`
	CreateDirs     bool   `json:"create_dirs"`
}

type listFilesResponse struct {
	Dir    string       `json:"dir"`
	Files  []fileEntry  `json:"files"`
}

type fileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModUnix int64  `json:"mod_unix"`
	IsDir   bool   `json:"is_dir"`
}

type readFileResponse struct {
	Path    string `json:"path"`
	Content string `json:"content_base64"`
}

// FilesWriteHandler writes sandbox-scoped files.
func FilesWriteHandler(cfg *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := bearerToken(r.Header.Get("Authorization"))
		if _, err := VerifyToken(token, cfg, ScopeFiles); err != nil {
			writeAuthError(w, err)
			return
		}

		var req writeFileRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}

		path, err := safeSandboxPath(req.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		data, err := base64.StdEncoding.DecodeString(req.ContentBase64)
		if err != nil {
			http.Error(w, "invalid base64 content", http.StatusBadRequest)
			return
		}

		if req.CreateDirs {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		mode := os.FileMode(req.Mode)
		if mode == 0 {
			mode = 0o644
		}

		if err := os.WriteFile(path, data, mode); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"path": path, "status": "ok"})
	})
}

// FilesReadHandler reads a single file as base64.
func FilesReadHandler(cfg *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := bearerToken(r.Header.Get("Authorization"))
		if _, err := VerifyToken(token, cfg, ScopeFiles); err != nil {
			writeAuthError(w, err)
			return
		}

		path, err := safeSandboxPath(r.URL.Query().Get("path"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		writeJSON(w, http.StatusOK, readFileResponse{
			Path:    path,
			Content: base64.StdEncoding.EncodeToString(raw),
		})
	})
}

// FilesListHandler lists directory entries (non-recursive).
func FilesListHandler(cfg *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := bearerToken(r.Header.Get("Authorization"))
		if _, err := VerifyToken(token, cfg, ScopeFiles); err != nil {
			writeAuthError(w, err)
			return
		}

		dir := strings.TrimSpace(r.URL.Query().Get("dir"))
		if dir == "" {
			dir = "/"
		}

		path, err := safeSandboxPath(dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := listFilesResponse{Dir: path, Files: make([]fileEntry, 0, len(entries))}
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil {
				continue
			}
			resp.Files = append(resp.Files, fileEntry{
				Name:    fi.Name(),
				Size:    fi.Size(),
				ModUnix: fi.ModTime().Unix(),
				IsDir:   fi.IsDir(),
			})
		}

		writeJSON(w, http.StatusOK, resp)
	})
}

func safeSandboxPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", os.ErrInvalid
	}

	clean := filepath.Clean(raw)
	if filepath.IsAbs(clean) {
		if strings.Contains(clean, "..") {
			return "", os.ErrInvalid
		}
		return clean, nil
	}

	joined := filepath.Clean(filepath.Join("/", clean))
	if strings.Contains(joined, "..") {
		return "", os.ErrInvalid
	}

	return joined, nil
}
