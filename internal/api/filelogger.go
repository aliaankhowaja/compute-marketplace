package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

const maxBodyCapture = 64 * 1024

type logEntry struct {
	Timestamp       string      `json:"timestamp"`
	Method          string      `json:"method"`
	Path            string      `json:"path"`
	Query           string      `json:"query,omitempty"`
	RequestHeaders  http.Header `json:"request_headers"`
	RequestBody     string      `json:"request_body,omitempty"`
	Status          int         `json:"status"`
	ResponseHeaders http.Header `json:"response_headers"`
	ResponseBody    string      `json:"response_body,omitempty"`
	DurationMs      int64       `json:"duration_ms"`
}

type bodyCapture struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (bc *bodyCapture) WriteHeader(code int) {
	bc.status = code
	bc.ResponseWriter.WriteHeader(code)
}

func (bc *bodyCapture) Write(b []byte) (int, error) {
	if bc.buf.Len() < maxBodyCapture {
		bc.buf.Write(b)
	}
	return bc.ResponseWriter.Write(b)
}

func (bc *bodyCapture) Flush() {
	if f, ok := bc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (bc *bodyCapture) Unwrap() http.ResponseWriter {
	return bc.ResponseWriter
}

func FileLogMiddleware(w io.Writer, mu *sync.Mutex, skipPaths []string) func(http.Handler) http.Handler {
	skip := make(map[string]struct{}, len(skipPaths))
	for _, p := range skipPaths {
		skip[p] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if _, ok := skip[r.URL.Path]; ok {
				next.ServeHTTP(rw, r)
				return
			}

			var reqBuf bytes.Buffer
			r.Body = io.NopCloser(io.TeeReader(r.Body, &reqBuf))

			bc := &bodyCapture{ResponseWriter: rw, status: http.StatusOK}
			start := time.Now()

			next.ServeHTTP(bc, r)

			io.Copy(io.Discard, r.Body)

			reqBody := reqBuf.String()
			if len(reqBody) > maxBodyCapture {
				reqBody = reqBody[:maxBodyCapture] + "...[truncated]"
			}
			respBody := bc.buf.String()
			if len(respBody) > maxBodyCapture {
				respBody = respBody[:maxBodyCapture] + "...[truncated]"
			}

			entry := logEntry{
				Timestamp:       time.Now().UTC().Format(time.RFC3339Nano),
				Method:          r.Method,
				Path:            r.URL.Path,
				Query:           r.URL.RawQuery,
				RequestHeaders:  r.Header,
				RequestBody:     reqBody,
				Status:          bc.status,
				ResponseHeaders: bc.Header(),
				ResponseBody:    respBody,
				DurationMs:      time.Since(start).Milliseconds(),
			}

			data, _ := json.Marshal(entry)
			mu.Lock()
			w.Write(append(data, '\n'))
			mu.Unlock()
		})
	}
}
