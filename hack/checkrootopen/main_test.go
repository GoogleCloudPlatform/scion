package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setupCleanFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/test\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "pkg/util/rootopen/rootopen_unix.go"), `package rootopen

import "os"

func withRootFD(dir *os.Root) (*os.File, error) {
	return dir.Open(".")
}
`)
	writeFile(t, filepath.Join(dir, "pkg/projectfs/open.go"), `package projectfs

import "os"

type Resolved struct{}

func (r Resolved) Open(create bool) (*os.Root, error) {
	return os.OpenRoot(".")
}
`)
	writeFile(t, filepath.Join(dir, "pkg/hub/ok.go"), `package hub

import (
	"os"

	"example.com/test/pkg/projectfs"
)

type otherFile struct{}

func (otherFile) Open() error { return nil }

func safe(r *os.Root, res projectfs.Resolved, fh otherFile) {
	// r.Open("commented") is ignored
	/* r.OpenFile("block-comment", 0, 0) is ignored */
	_ = 1 // r.Create("trailing") is ignored
	_, _ = os.Open("plain")
	_, _ = os.OpenFile("plain", 0, 0)
	_, _ = os.Create("plain")
	_, _ = res.Open(false)
	_ = fh.Open()
}
`)
	// Test files are skipped.
	writeFile(t, filepath.Join(dir, "pkg/hub/ok_test.go"), `package hub

import "os"

func t(r *os.Root) { _, _ = r.Open("x") }
`)
	allow := filepath.Join(dir, "allow")
	writeFile(t, allow, `# fixture
pkg/util/rootopen/rootopen_unix.go | withRootFD | 1 | helper opens root fd itself
`)
	return dir, allow
}

func TestScan_CleanAndViolations(t *testing.T) {
	t.Run("clean_allowlisted_tree", func(t *testing.T) {
		dir, allow := setupCleanFixture(t)
		rc, _, errOut := scan(dir, allow, "fixture")
		if rc != 0 {
			t.Fatalf("clean tree rc = %d, want 0; stderr:\n%s", rc, errOut)
		}
	})

	cases := []struct {
		name     string
		relPath  string
		src      string
		wantText string
	}{
		{
			name:    "direct_root_open_openfile_create",
			relPath: "pkg/hub/bad_direct.go",
			src: `package hub
import "os"
func badDirect(r *os.Root) {
	_, _ = r.Open("a")
	_, _ = r.OpenFile("b", os.O_RDONLY, 0)
	_, _ = r.Create("c")
}
`,
			wantText: "pkg/hub/bad_direct.go|badDirect: 3 (allowlist 0)",
		},
		{
			name:    "os_OpenInRoot",
			relPath: "pkg/hub/bad_openinroot.go",
			src: `package hub
import "os"
func badOpenInRoot(dir string) {
	_, _ = os.OpenInRoot(dir, "x")
}
`,
			wantText: "pkg/hub/bad_openinroot.go|badOpenInRoot: 1 (allowlist 0)",
		},
		{
			name:    "os_import_alias_root_and_OpenInRoot",
			relPath: "pkg/hub/bad_alias.go",
			src: `package hub
import sys "os"
func badAlias(r *sys.Root) {
	_, _ = r.Open("x")
	_, _ = sys.OpenInRoot("/tmp", "y")
}
`,
			wantText: "pkg/hub/bad_alias.go|badAlias: 2 (allowlist 0)",
		},
		{
			name:    "indirect_root_via_projectfs_Resolved_Open_without_os_import",
			relPath: "pkg/hub/bad_indirect.go",
			src: `package hub
import "example.com/test/pkg/projectfs"
func badIndirect(res projectfs.Resolved) {
	r, err := res.Open(false)
	if err != nil {
		return
	}
	_, _ = r.Open("x")
}
`,
			wantText: "pkg/hub/bad_indirect.go|badIndirect: 1 (allowlist 0)",
		},
		{
			name:    "method_value",
			relPath: "pkg/hub/bad_method_val.go",
			src: `package hub
import "os"
func badMethodVal(r *os.Root) {
	fn := r.Open
	_, _ = fn("x")
}
`,
			wantText: "pkg/hub/bad_method_val.go|badMethodVal: 1 (allowlist 0)",
		},
		{
			name:    "method_expression",
			relPath: "pkg/hub/bad_method_expr.go",
			src: `package hub
import sys "os"
func badMethodExpr(r *sys.Root) {
	_, _ = (*sys.Root).Open(r, "x")
	_, _ = (*sys.Root).Create(r, "y")
}
`,
			wantText: "pkg/hub/bad_method_expr.go|badMethodExpr: 2 (allowlist 0)",
		},
		{
			name:    "chained_calls",
			relPath: "pkg/hub/bad_chained.go",
			src: `package hub
import "os"
func getRoot() *os.Root { return nil }
func badChained() {
	_, _ = getRoot().Open("x")
}
`,
			wantText: "pkg/hub/bad_chained.go|badChained: 1 (allowlist 0)",
		},
		{
			name:    "struct_field_root",
			relPath: "pkg/hub/bad_field.go",
			src: `package hub
import "os"
type holder struct { root *os.Root }
func (h *holder) badField() {
	_, _ = h.root.OpenFile("x", os.O_RDONLY, 0)
}
`,
			wantText: "pkg/hub/bad_field.go|badField: 1 (allowlist 0)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, allow := setupCleanFixture(t)
			writeFile(t, filepath.Join(dir, tc.relPath), tc.src)
			rc, _, errOut := scan(dir, allow, "fixture")
			if rc != 1 || !strings.Contains(errOut, tc.wantText) {
				t.Fatalf("rc = %d (want 1); stderr:\n%s\nwant substring: %q", rc, errOut, tc.wantText)
			}
		})
	}
}

func TestScan_ErrorModes(t *testing.T) {
	t.Run("stale_allowlist_entry", func(t *testing.T) {
		dir, allow := setupCleanFixture(t)
		writeFile(t, allow, `pkg/util/rootopen/rootopen_unix.go | withRootFD | 2 | stale count
`)
		rc, _, errOut := scan(dir, allow, "fixture")
		if rc != 1 || !strings.Contains(errOut, "allowlist is stale") {
			t.Fatalf("rc = %d (want 1); stderr:\n%s", rc, errOut)
		}
	})

	t.Run("malformed_allowlist_line", func(t *testing.T) {
		dir, allow := setupCleanFixture(t)
		writeFile(t, allow, "pkg/util/rootopen/rootopen_unix.go | withRootFD | 1\n")
		rc, _, errOut := scan(dir, allow, "fixture")
		if rc != 1 || !strings.Contains(errOut, "allowlist parse error at line 1") {
			t.Fatalf("rc = %d (want 1); stderr:\n%s", rc, errOut)
		}
	})

	t.Run("missing_scan_root", func(t *testing.T) {
		dir, allow := setupCleanFixture(t)
		rc, _, _ := scan(filepath.Join(dir, "missing"), allow, "fixture")
		if rc != 4 {
			t.Fatalf("rc = %d, want 4", rc)
		}
	})

	t.Run("empty_scan_zero_calls", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
		allow := filepath.Join(dir, "allow")
		writeFile(t, allow, "# empty\n")
		rc, _, _ := scan(dir, allow, "fixture")
		if rc != 4 {
			t.Fatalf("rc = %d, want 4", rc)
		}
	})

	t.Run("parse_error_returns_3", func(t *testing.T) {
		dir, allow := setupCleanFixture(t)
		writeFile(t, filepath.Join(dir, "pkg/hub/broken.go"), "package hub\nfunc {\n")
		rc, _, _ := scan(dir, allow, "fixture")
		if rc != 3 {
			t.Fatalf("rc = %d, want 3", rc)
		}
	})
}
