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
//
// After logical resolution, symlinks are resolved to ensure the real on-disk path
// stays within the sandbox root, preventing symlink escape attacks.
func resolveSandboxPath(root, userPath string) (string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return "", os.ErrInvalid
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", os.ErrInvalid
	}
	root = realRoot

	raw := strings.TrimSpace(userPath)
	if raw == "" {
		return "", os.ErrInvalid
	}

	var full string

	// Accept absolute paths that already lie under root (e.g. callers passing SANDBOX_ROOT/child).
	if filepath.IsAbs(raw) {
		cleaned := filepath.Clean(raw)
		sep := string(filepath.Separator)
		if cleaned == root {
			full = root
		} else if strings.HasPrefix(cleaned, root+sep) {
			rel, err := filepath.Rel(root, cleaned)
			if err != nil {
				return "", os.ErrInvalid
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+sep) {
				return "", os.ErrInvalid
			}
			full = cleaned
		} else {
			// Absolute path not under root — treat as relative (strip leading slashes).
			goto relative
		}
	} else {
		goto relative
	}
	goto resolve

relative:
	for strings.HasPrefix(raw, "/") {
		raw = raw[1:]
	}
	{
		p := filepath.Clean(raw)
		if p == "." {
			return root, nil
		}
		if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			return "", os.ErrInvalid
		}
		full = filepath.Clean(filepath.Join(root, p))
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return "", os.ErrInvalid
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", os.ErrInvalid
		}
	}

resolve:

	// Resolve symlinks to get the real on-disk path and re-validate containment.
	// Walk up from full until we find an existing ancestor, resolve its symlinks,
	// then re-append the non-existent tail components.
	realFull := evalSymlinksWalk(full)

	if realFull != root {
		sep := string(filepath.Separator)
		if !strings.HasPrefix(realFull, root+sep) {
			return "", os.ErrInvalid
		}
	}

	return full, nil
}

// evalSymlinksWalk resolves symlinks for the longest existing prefix of path,
// then appends the remaining non-existent components. This handles write paths
// where the target file doesn't exist yet but parent directories might contain symlinks.
func evalSymlinksWalk(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}

	// Walk up to find the longest existing ancestor.
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if dir == path {
		return path
	}
	return filepath.Join(evalSymlinksWalk(dir), base)
}
