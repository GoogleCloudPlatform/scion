// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build unix

package maintenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readLink(t *testing.T, root, name string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("readlink %s: %v", name, err)
	}
	return target
}

func TestSymlinkInstaller_Preflight(t *testing.T) {
	t.Run("usable layout", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		if err := newSymlinkInstaller(symlinkOpts(root)).Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})

	t.Run("binary outside releases", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		opts := symlinkOpts(root)
		opts.Executable = func() (string, error) { return writeBin(t, "x"), nil }
		err := newSymlinkInstaller(opts).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "is not under") {
			t.Fatalf("Preflight = %v, want 'is not under'", err)
		}
	})

	t.Run("relative root", func(t *testing.T) {
		err := newSymlinkInstaller(symlinkOpts("opt/scion")).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not an absolute path") {
			t.Fatalf("Preflight = %v", err)
		}
	})

	t.Run("missing root", func(t *testing.T) {
		opts := symlinkOpts(filepath.Join(t.TempDir(), "nope"))
		if err := newSymlinkInstaller(opts).Preflight(context.Background()); err == nil {
			t.Fatal("Preflight succeeded without an install root")
		}
	})

	t.Run("current is not a symlink", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		opts := symlinkOpts(root)
		opts.Executable = func() (string, error) {
			return filepath.Join(root, releasesDirName, "nightly-20261022", binaryName), nil
		}
		cur := filepath.Join(root, currentLinkName)
		if err := os.Remove(cur); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			t.Fatal(err)
		}
		err := newSymlinkInstaller(opts).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not a symlink") {
			t.Fatalf("Preflight = %v, want 'not a symlink'", err)
		}
	})

	t.Run("current owned by another user", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		opts := symlinkOpts(root)
		opts.UID = func() int { return os.Getuid() + 1 }
		err := newSymlinkInstaller(opts).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not owned by the hub user") {
			t.Fatalf("Preflight = %v, want ownership error", err)
		}
	})

	t.Run("releases not writable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can write read-only directories")
		}
		root := installLayout(t, "nightly-20261022")
		rel := filepath.Join(root, releasesDirName)
		if err := os.Chmod(rel, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(rel, 0o755) })
		err := newSymlinkInstaller(symlinkOpts(root)).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not writable") {
			t.Fatalf("Preflight = %v, want 'not writable'", err)
		}
	})
}

func TestSymlinkInstaller_StageActivate(t *testing.T) {
	ctx := context.Background()
	root := installLayout(t, "nightly-20261022")
	inst := newSymlinkInstaller(symlinkOpts(root))

	staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if want := filepath.Join(root, releasesDirName, "nightly-20261023"); staged != want {
		t.Fatalf("staged = %q, want %q", staged, want)
	}
	if _, err := os.Stat(staged + partialSuffix); !os.IsNotExist(err) {
		t.Fatalf("partial directory left behind: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(staged, binaryName))
	if err != nil || string(got) != "new" {
		t.Fatalf("staged binary = %q, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(staged, binaryName)); fi.Mode().Perm() != 0o755 {
		t.Fatalf("staged mode = %v, want 0755", fi.Mode().Perm())
	}
	// Staging does not touch current.
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current after Stage = %q", cur)
	}

	if err := inst.Activate(ctx, staged); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261023" {
		t.Fatalf("current = %q, want relative releases/nightly-20261023", cur)
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261022" {
		t.Fatalf("previous = %q, want releases/nightly-20261022", prev)
	}
	if _, err := os.Lstat(filepath.Join(root, currentLinkName+".new")); !os.IsNotExist(err) {
		t.Fatal("current.new left behind")
	}
	// The resolved binary through current is the new one.
	got, err = os.ReadFile(filepath.Join(root, currentLinkName, binaryName))
	if err != nil || string(got) != "new" {
		t.Fatalf("current/scion = %q, %v", got, err)
	}

	rels := inst.Retained()
	if len(rels) != 2 {
		t.Fatalf("Retained = %+v, want 2 releases", rels)
	}
	flags := map[string][2]bool{}
	for _, r := range rels {
		flags[r.Tag] = [2]bool{r.Current, r.Previous}
	}
	if flags["nightly-20261023"] != [2]bool{true, false} || flags["nightly-20261022"] != [2]bool{false, true} {
		t.Fatalf("Retained flags = %v", flags)
	}
}

func TestSymlinkInstaller_StageRefusals(t *testing.T) {
	ctx := context.Background()
	root := installLayout(t, "nightly-20261022")
	inst := newSymlinkInstaller(symlinkOpts(root))
	bin := writeBin(t, "x")

	if _, err := inst.Stage(ctx, "nightly-20261022", bin); err == nil {
		t.Fatal("Stage replaced the active release")
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", ".hidden", "x.partial"} {
		if _, err := inst.Stage(ctx, bad, bin); err == nil {
			t.Errorf("Stage(%q) succeeded", bad)
		}
	}
	if err := inst.Activate(ctx, filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Fatal("Activate accepted a directory outside releases/")
	}
	if err := inst.Activate(ctx, filepath.Join(root, releasesDirName, "missing")); err == nil {
		t.Fatal("Activate accepted a release without a binary")
	}
}

func TestSymlinkInstaller_StageReplacesStalePartialAndRetainedCopy(t *testing.T) {
	ctx := context.Background()
	root := installLayout(t, "nightly-20261022", "nightly-20261020")
	partial := filepath.Join(root, releasesDirName, "nightly-20261020"+partialSuffix)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := newSymlinkInstaller(symlinkOpts(root))
	staged, err := inst.Stage(ctx, "nightly-20261020", writeBin(t, "fresh"))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(staged, binaryName))
	if string(got) != "fresh" {
		t.Fatalf("retained copy not replaced: %q", got)
	}
}

func TestSymlinkInstaller_Prune(t *testing.T) {
	ctx := context.Background()
	// current = r00 (oldest); r01..r07 are older installs. previous will be
	// r00 after activating new.
	tags := []string{"nightly-20261000"}
	for i := 1; i <= 7; i++ {
		tags = append(tags, fmt.Sprintf("nightly-202610%02d", i))
	}
	root := installLayout(t, tags...)
	base := time.Now().Add(-time.Hour)
	for i, tag := range tags {
		// r00 (current) is the oldest; r07 the newest.
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(root, releasesDirName, tag), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	inst := newSymlinkInstaller(symlinkOpts(root))
	staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Activate(ctx, staged); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, r := range inst.Retained() {
		got = append(got, r.Tag)
	}
	// Newest 5: new, r07, r06, r05, r04; plus previous (r00) which is old.
	want := map[string]bool{
		"nightly-20261023": true, "nightly-20261007": true, "nightly-20261006": true,
		"nightly-20261005": true, "nightly-20261004": true, "nightly-20261000": true,
	}
	if len(got) != len(want) {
		t.Fatalf("retained %v, want %v", got, want)
	}
	for _, tag := range got {
		if !want[tag] {
			t.Fatalf("retained %v, want %v", got, want)
		}
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261000" {
		t.Fatalf("previous = %q", prev)
	}
}

func TestSymlinkInstaller_Deactivate(t *testing.T) {
	ctx := context.Background()
	root := installLayout(t, "nightly-20261022")
	inst := newSymlinkInstaller(symlinkOpts(root))
	if err := inst.Deactivate(ctx); err == nil {
		t.Fatal("Deactivate without an activation succeeded")
	}
	staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Activate(ctx, staged); err != nil {
		t.Fatal(err)
	}
	if err := inst.Deactivate(ctx); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current = %q", cur)
	}
	// There was no previous before the activation, so none is left.
	if _, err := os.Lstat(filepath.Join(root, prevLinkName)); !os.IsNotExist(err) {
		t.Fatalf("previous left behind: %v", err)
	}
	if err := inst.Deactivate(ctx); err == nil {
		t.Fatal("second Deactivate succeeded")
	}
}

func TestSymlinkInstaller_Restart(t *testing.T) {
	called := false
	opts := symlinkOpts(t.TempDir())
	opts.Signal = func() error { called = true; return nil }
	if err := newSymlinkInstaller(opts).Restart(context.Background()); err != nil || !called {
		t.Fatalf("Restart = %v, signalled = %v", err, called)
	}
}

// replaceLink points <root>/<name> at target (creating or replacing it).
func replaceLink(t *testing.T, root, name, target string) {
	t.Helper()
	p := filepath.Join(root, name)
	_ = os.Remove(p)
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkInstaller_LinkValidation(t *testing.T) {
	ctx := context.Background()
	outside := t.TempDir() // a real directory outside the install root

	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string
	}{
		{"previous outside releases (absolute)", func(t *testing.T, root string) {
			replaceLink(t, root, prevLinkName, outside)
		}, "resolves outside"},
		{"previous escaping with ..", func(t *testing.T, root string) {
			replaceLink(t, root, prevLinkName, "releases/../..")
		}, "resolves outside"},
		{"previous nested below a release", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, releasesDirName, "nightly-20261021", "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			replaceLink(t, root, prevLinkName, "releases/nightly-20261021/sub")
		}, "resolves outside"},
		{"previous via a symlink that leaves releases", func(t *testing.T, root string) {
			if err := os.Symlink(outside, filepath.Join(root, releasesDirName, "sneaky")); err != nil {
				t.Fatal(err)
			}
			replaceLink(t, root, prevLinkName, "releases/sneaky")
		}, "resolves outside"},
		{"previous is the install root", func(t *testing.T, root string) {
			replaceLink(t, root, prevLinkName, ".")
		}, "resolves outside"},
		{"previous is the releases directory", func(t *testing.T, root string) {
			replaceLink(t, root, prevLinkName, "releases")
		}, "resolves outside"},
		{"previous dangling", func(t *testing.T, root string) {
			replaceLink(t, root, prevLinkName, "releases/gone")
		}, "does not resolve"},
		{"previous points at a file directly under releases", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, releasesDirName, "stray"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			replaceLink(t, root, prevLinkName, "releases/stray")
		}, "not a release directory"},
		{"previous is a directory", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, prevLinkName), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "not a symlink"},
		{"previous points at a file", func(t *testing.T, root string) {
			replaceLink(t, root, prevLinkName, "releases/nightly-20261021/scion")
		}, "resolves outside"},
		{"current outside releases", func(t *testing.T, root string) {
			replaceLink(t, root, currentLinkName, outside)
		}, "resolves outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := installLayout(t, "nightly-20261022", "nightly-20261021")
			// Keep the executable resolvable under releases/ even when the
			// test repoints current.
			opts := symlinkOpts(root)
			opts.Executable = func() (string, error) {
				return filepath.Join(root, releasesDirName, "nightly-20261022", binaryName), nil
			}
			tt.setup(t, root)
			inst := newSymlinkInstaller(opts)

			if err := inst.Preflight(ctx); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Preflight = %v, want %q", err, tt.want)
			}

			// Activate re-validates (Preflight may be stale) and changes nothing.
			before, _ := os.Readlink(filepath.Join(root, currentLinkName))
			staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
			if err != nil {
				t.Fatal(err)
			}
			err = inst.Activate(ctx, staged)
			if err == nil || !strings.Contains(err.Error(), "refusing to activate") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Activate = %v, want refusal with %q", err, tt.want)
			}
			if after, _ := os.Readlink(filepath.Join(root, currentLinkName)); after != before {
				t.Fatalf("current changed from %q to %q", before, after)
			}
		})
	}

	t.Run("invalid previous error says how to fix it", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		replaceLink(t, root, prevLinkName, "releases/gone")
		err := newSymlinkInstaller(symlinkOpts(root)).Preflight(ctx)
		prev := filepath.Join(root, prevLinkName)
		if err == nil || !strings.Contains(err.Error(), "repoint "+prev) || !strings.Contains(err.Error(), "or remove it") ||
			!strings.Contains(err.Error(), "as the hub user") {
			t.Fatalf("Preflight = %v, want an actionable fix for %s", err, prev)
		}
	})

	t.Run("valid previous passes", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022", "nightly-20261021")
		replaceLink(t, root, prevLinkName, "releases/nightly-20261021")
		if err := newSymlinkInstaller(symlinkOpts(root)).Preflight(ctx); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})

	t.Run("previous owned by another user", func(t *testing.T) {
		// The owner check is shared with current; checkReleaseLink is
		// called directly so current's check does not fire first.
		root := installLayout(t, "nightly-20261022", "nightly-20261021")
		replaceLink(t, root, prevLinkName, "releases/nightly-20261021")
		opts := symlinkOpts(root)
		opts.UID = func() int { return os.Getuid() + 1 }
		_, err := newSymlinkInstaller(opts).checkReleaseLink(prevLinkName)
		if err == nil || !strings.Contains(err.Error(), "not owned by the hub user") {
			t.Fatalf("checkReleaseLink = %v", err)
		}
	})
}

func TestSymlinkInstaller_DeactivateValidates(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (string, *symlinkInstaller) {
		t.Helper()
		root := installLayout(t, "nightly-20261022", "nightly-20261021")
		replaceLink(t, root, prevLinkName, "releases/nightly-20261021")
		inst := newSymlinkInstaller(symlinkOpts(root))
		staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
		if err != nil {
			t.Fatal(err)
		}
		if err := inst.Activate(ctx, staged); err != nil {
			t.Fatal(err)
		}
		return root, inst
	}

	t.Run("previous tampered after activation", func(t *testing.T) {
		root, inst := setup(t)
		replaceLink(t, root, prevLinkName, t.TempDir())
		err := inst.Deactivate(ctx)
		if err == nil || !strings.Contains(err.Error(), "refusing to roll back") {
			t.Fatalf("Deactivate = %v, want refusal", err)
		}
		if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261023" {
			t.Fatalf("current changed by a refused rollback: %q", cur)
		}
	})

	t.Run("old current release removed", func(t *testing.T) {
		root, inst := setup(t)
		if err := os.RemoveAll(filepath.Join(root, releasesDirName, "nightly-20261022")); err != nil {
			t.Fatal(err)
		}
		// previous points at the removed release too; drop it so the
		// refusal comes from the restore target check.
		if err := os.Remove(filepath.Join(root, prevLinkName)); err != nil {
			t.Fatal(err)
		}
		err := inst.Deactivate(ctx)
		if err == nil || !strings.Contains(err.Error(), "refusing to roll back") || !strings.Contains(err.Error(), "does not resolve") {
			t.Fatalf("Deactivate = %v, want refusal", err)
		}
		if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261023" {
			t.Fatalf("current = %q", cur)
		}
	})

	// The pre-activation previous target is re-checked before it is written
	// back: if that release was replaced or removed meanwhile, previous is
	// dropped instead of pointing at it.
	for name, tamper := range map[string]func(t *testing.T, root string){
		"old previous release replaced by an outside symlink": func(t *testing.T, root string) {
			rel := filepath.Join(root, releasesDirName, "nightly-20261021")
			if err := os.RemoveAll(rel); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), rel); err != nil {
				t.Fatal(err)
			}
		},
		"old previous release removed": func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, releasesDirName, "nightly-20261021")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, inst := setup(t)
			tamper(t, root)
			if err := inst.Deactivate(ctx); err != nil {
				t.Fatalf("Deactivate: %v", err)
			}
			if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
				t.Fatalf("current = %q", cur)
			}
			if target, err := os.Readlink(filepath.Join(root, prevLinkName)); err == nil {
				t.Fatalf("previous restored to the invalid target %q", target)
			}
		})
	}

	t.Run("valid links roll back", func(t *testing.T) {
		root, inst := setup(t)
		if err := inst.Deactivate(ctx); err != nil {
			t.Fatalf("Deactivate: %v", err)
		}
		if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
			t.Fatalf("current = %q", cur)
		}
		if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261021" {
			t.Fatalf("previous = %q", prev)
		}
	})
}
