package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSandboxPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "box")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		user    string
		want    string
		wantErr bool
	}{
		{"empty", "", "", true},
		{"rel", "a/b", filepath.Join(root, "a", "b"), false},
		{"abs_api", "/a/b", filepath.Join(root, "a", "b"), false},
		{"root_slash", "/", root, false},
		{"dot", ".", root, false},
		{"dotdot", "../x", "", true},
		{"nested_escape", "a/../../x", "", true},
		{"logical_workspace", "/workspace", filepath.Join(root, "workspace"), false},
		{"fs_abs_equal_root", root, root, false},
		{"fs_abs_child", filepath.Join(root, "deep"), filepath.Join(root, "deep"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSandboxPath(root, tc.user)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			rel, err := filepath.Rel(root, got)
			if err != nil {
				t.Fatal(err)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("path escapes root: got=%q rel=%q", got, rel)
			}
		})
	}
}

func TestResolveSandboxPath_SymlinkEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sandbox")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a symlink inside the sandbox that points outside it.
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("sensitive"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Symlink: <sandbox>/escape -> <outside>
	symlinkPath := filepath.Join(root, "escape")
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// Attempting to resolve "escape/secret.txt" should fail — the real path is outside root.
	_, err := resolveSandboxPath(root, "escape/secret.txt")
	if err == nil {
		t.Fatal("expected error for symlink escaping sandbox, got nil")
	}

	// Direct symlink to a file outside.
	fileSymlink := filepath.Join(root, "passwd")
	if err := os.Symlink("/etc/passwd", fileSymlink); err != nil {
		t.Fatal(err)
	}
	_, err = resolveSandboxPath(root, "passwd")
	if err == nil {
		t.Fatal("expected error for symlink to /etc/passwd, got nil")
	}

	// A symlink that stays WITHIN the sandbox should succeed.
	subdir := filepath.Join(root, "real")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	internalLink := filepath.Join(root, "link_to_real")
	if err := os.Symlink(subdir, internalLink); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSandboxPath(root, "link_to_real")
	if err != nil {
		t.Fatalf("internal symlink should be allowed, got error: %v", err)
	}
	// The function returns the logical path; security is enforced by validating
	// the resolved real path stays within root.
	if got != internalLink {
		t.Fatalf("expected %q, got %q", internalLink, got)
	}
}
