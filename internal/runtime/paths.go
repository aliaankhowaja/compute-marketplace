package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// resolveSandboxPath maps a logical API path to an absolute path confined under root.
// Paths are always interpreted relative to root (leading slashes are stripped).
func resolveSandboxPath(root, userPath string) (string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return "", os.ErrInvalid
	}
	raw := strings.TrimSpace(userPath)
	if raw == "" {
		return "", os.ErrInvalid
	}
	for strings.HasPrefix(raw, "/") {
		raw = raw[1:]
	}
	p := filepath.Clean(raw)
	if p == "." {
		return root, nil
	}
	if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return "", os.ErrInvalid
	}
	full := filepath.Join(root, p)
	full = filepath.Clean(full)
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", os.ErrInvalid
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", os.ErrInvalid
	}
	return full, nil
}
