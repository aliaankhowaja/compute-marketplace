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
		wantErr bool
	}{
		{"empty", "", true},
		{"rel", "a/b", false},
		{"abs_api", "/a/b", false},
		{"root_slash", "/", false},
		{"dotdot", "../x", true},
		{"nested_escape", "a/../../x", true},
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
