package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// resolveSandboxPath maps a logical API path to an absolute path confined under root.
// Paths are normally interpreted relative to root (leading slashes are stripped). If the
// input is an absolute filesystem path that already lies under root, it is accepted as-is
// (after Clean) so callers can pass SANDBOX_ROOT explicitly.
func resolveSandboxPath(root, userPath string) (string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return "", os.ErrInvalid
	}
	raw := strings.TrimSpace(userPath)
	if raw == "" {
		return "", os.ErrInvalid
	}

	if filepath.IsAbs(raw) {
		full := filepath.Clean(raw)
		if full == root {
			return full, nil
		}
		sep := string(filepath.Separator)
		if strings.HasPrefix(full, root+sep) {
			rel, err := filepath.Rel(root, full)
			if err != nil {
				return "", os.ErrInvalid
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+sep) {
				return "", os.ErrInvalid
			}
			return full, nil
		}
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
