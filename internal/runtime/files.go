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

		path, err := resolveSandboxPath(cfg.SandboxRoot, req.Path)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		data, err := base64.StdEncoding.DecodeString(req.ContentBase64)
		if err != nil {
			http.Error(w, "invalid base64 content", http.StatusBadRequest)
			return
		}

		if req.CreateDirs {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				http.Error(w, "failed to create directories", http.StatusInternalServerError)
				return
			}
		}

		mode := os.FileMode(req.Mode) & 0o7777
		if mode == 0 {
			mode = 0o644
		}
		// Strip setuid, setgid, sticky bits and cap at 0755.
		mode &^= os.ModeSetuid | os.ModeSetgid | os.ModeSticky
		if mode > 0o755 {
			mode = 0o755
		}

		if err := os.WriteFile(path, data, mode); err != nil {
			http.Error(w, "failed to write file", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"path": path, "status": "ok"})
	})
}

const maxReadFileBytes = 50 << 20 // 50 MB

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

		path, err := resolveSandboxPath(cfg.SandboxRoot, r.URL.Query().Get("path"))
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		fi, err := os.Stat(path)
		if err != nil {
			http.Error(w, "file not found", http.StatusBadRequest)
			return
		}
		if fi.Size() > maxReadFileBytes {
			http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
			return
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, "failed to read file", http.StatusBadRequest)
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

		path, err := resolveSandboxPath(cfg.SandboxRoot, dir)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			http.Error(w, "directory not found", http.StatusBadRequest)
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
