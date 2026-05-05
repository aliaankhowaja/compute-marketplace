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
