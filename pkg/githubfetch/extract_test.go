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

package githubfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTarGz_DirectoryEntryCap(t *testing.T) {
	const sha = "cccccccccccccccccccccccccccccccccccccccc"

	t.Run("many empty dirs exceeding cap rejected with KindTooLarge", func(t *testing.T) {
		lim := Limits{
			MaxFiles:     3, // directory cap = 4 * 3 = 12
			MaxFileBytes: 1024,
			MaxTotal:     1024,
			MaxUnpacked:  1 << 20,
			MaxNameBytes: 256,
		}
		entries := make([]tarTestEntry, 0, 14)
		for i := 0; i < 13; i++ {
			entries = append(entries, tarTestEntry{
				name:     fmt.Sprintf("repo-sha/t/gitlink-%02d/", i),
				typeflag: tar.TypeDir,
			})
		}
		tarData := buildTestTarGz(t, sha, entries)
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		_, _, _, err = extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, nil, root, lim)
		assertErrorKind(t, err, KindTooLarge)
		var ghe *Error
		if !errors.As(err, &ghe) || !strings.Contains(ghe.Hint, "directory limit") {
			t.Fatalf("expected directory limit hint, got: %v", err)
		}
	})

	t.Run("normal folder at directory cap succeeds", func(t *testing.T) {
		lim := Limits{
			MaxFiles:     3, // directory cap = 12
			MaxFileBytes: 1024,
			MaxTotal:     1024,
			MaxUnpacked:  1 << 20,
			MaxNameBytes: 256,
		}
		entries := make([]tarTestEntry, 0, 15)
		for i := 0; i < 12; i++ {
			entries = append(entries, tarTestEntry{
				name:     fmt.Sprintf("repo-sha/t/dir-%02d/", i),
				typeflag: tar.TypeDir,
			})
		}
		entries = append(entries,
			tarTestEntry{name: "repo-sha/t/dir-00/a.txt", body: []byte("a")},
			tarTestEntry{name: "repo-sha/t/dir-01/b.txt", body: []byte("b")},
			tarTestEntry{name: "repo-sha/t/dir-02/c.txt", body: []byte("c")},
		)
		tarData := buildTestTarGz(t, sha, entries)
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		files, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, nil, root, lim)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if files != 3 {
			t.Fatalf("files = %d, want 3", files)
		}
	})
}

func TestExtractTarGz_PolicyRejectLinks(t *testing.T) {
	const sha = "cccccccccccccccccccccccccccccccccccccccc"

	for _, tc := range []struct {
		name  string
		entry tarTestEntry
	}{
		{
			name:  "symlink rejected",
			entry: tarTestEntry{name: "repo-sha/t/sym", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		},
		{
			name:  "hardlink rejected",
			entry: tarTestEntry{name: "repo-sha/t/hard", typeflag: tar.TypeLink, linkname: "repo-sha/t/a.txt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tarData := buildTestTarGz(t, sha, []tarTestEntry{
				{name: "repo-sha/t/a.txt", body: []byte("ok")},
				tc.entry,
			})
			lim := ImportHostedLimits()
			lim.OnSymlink = PolicyReject
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()

			_, _, _, err = extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, nil, root, lim)
			assertErrorKind(t, err, KindUnsupportedContent)
		})
	}
}

func TestExtractTarGz_SelectorMode(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const otherSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	sel := func(rel string) []string {
		if rest, ok := strings.CutPrefix(rel, "skills/tdd/"); ok && rest != "" {
			return []string{"coord/skills/tdd/" + rest, "dev/skills/tdd/" + rest}
		}
		return nil
	}
	baseOpts := func(budget *ExtractBudget) TarExtractOptions {
		return TarExtractOptions{
			Repo:              "acme/skills",
			ExpectedSHA:       sha,
			RequireLeadingPAX: true,
			Select:            sel,
			Roots:             []string{"skills/tdd"},
			SkipEmptyDirs:     true,
			DirMode:           0o700,
			Budget:            budget,
			Limits: Limits{
				MaxFiles:     10,
				MaxFileBytes: 1024,
				MaxTotal:     2048,
				MaxUnpacked:  1 << 20,
				OnSymlink:    PolicyReject,
			},
		}
	}

	t.Run("multi-destination extraction, modes, skipped counts, and shared budget", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/", typeflag: tar.TypeDir},
			{name: "repo-sha/CLAUDE.md", typeflag: tar.TypeSymlink, linkname: "AGENTS.md"},
			{name: "repo-sha/docs/dev", typeflag: tar.TypeChar},
			{name: "repo-sha/skills/", typeflag: tar.TypeDir},
			{name: "repo-sha/skills/tdd/", typeflag: tar.TypeDir},
			{name: "repo-sha/skills/tdd/SKILL.md", body: []byte("tdd\n"), mode: 0o600},
			{name: "repo-sha/skills/tdd/run.sh", body: []byte("#!/bin/sh\n"), mode: 0o755},
		})
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		budget := &ExtractBudget{}
		res, err := ExtractTarGz(bytes.NewReader(tarData), root, baseOpts(budget))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Files != 4 || budget.Files != 4 {
			t.Fatalf("files = %d / budget %d, want 4", res.Files, budget.Files)
		}
		wantBytes := int64(2 * (len("tdd\n") + len("#!/bin/sh\n")))
		if res.Bytes != wantBytes || budget.Bytes != wantBytes {
			t.Fatalf("bytes = %d / budget %d, want %d", res.Bytes, budget.Bytes, wantBytes)
		}
		if res.Skipped.Symlinks != 1 || res.Skipped.Other != 1 {
			t.Fatalf("skipped = %+v, want 1 symlink and 1 other", res.Skipped)
		}
		for _, tpl := range []string{"coord", "dev"} {
			b, err := os.ReadFile(filepath.Join(dir, tpl, "skills", "tdd", "SKILL.md"))
			if err != nil || string(b) != "tdd\n" {
				t.Fatalf("%s SKILL.md = %q, %v", tpl, string(b), err)
			}
			fi, err := os.Stat(filepath.Join(dir, tpl, "skills", "tdd", "run.sh"))
			if err != nil || fi.Mode().Perm() != 0o755 {
				t.Fatalf("%s run.sh mode = %v, %v", tpl, fi.Mode().Perm(), err)
			}
			fi, err = os.Stat(filepath.Join(dir, tpl, "skills", "tdd", "SKILL.md"))
			if err != nil || fi.Mode().Perm() != 0o644 {
				t.Fatalf("%s SKILL.md mode = %v, %v", tpl, fi.Mode().Perm(), err)
			}
		}

		// Second call sharing the same budget trips MaxFiles when cumulative count exceeds limit.
		opts2 := baseOpts(budget)
		opts2.Limits.MaxFiles = 5 // already at 4; writing 2 more exceeds 5
		tarData2 := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/skills/tdd/extra.md", body: []byte("more")},
		})
		_, err = ExtractTarGz(bytes.NewReader(tarData2), root, opts2)
		assertErrorKind(t, err, KindTooLarge)
	})

	t.Run("RequireLeadingPAX rejects missing and mismatched PAX before writing", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			paxSHA  string
			wantErr error
		}{
			{name: "missing", paxSHA: "", wantErr: ErrPAXMissing},
			{name: "mismatch", paxSHA: otherSHA, wantErr: ErrPAXMismatch},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tarData := buildTestTarGz(t, tc.paxSHA, []tarTestEntry{
					{name: "repo-sha/skills/tdd/SKILL.md", body: []byte("x")},
				})
				dir := t.TempDir()
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()

				_, err = ExtractTarGz(bytes.NewReader(tarData), root, baseOpts(&ExtractBudget{}))
				assertErrorKind(t, err, KindUpstream)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("expected empty dir, got %v (err=%v)", entries, err)
				}
			})
		}
	})

	t.Run("second PAX global header rejected", func(t *testing.T) {
		var buf bytes.Buffer
		gzw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gzw)
		for range 2 {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag:   tar.TypeXGlobalHeader,
				Name:       "pax_global_header",
				PAXRecords: map[string]string{"comment": sha},
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		_, err = ExtractTarGz(bytes.NewReader(buf.Bytes()), root, baseOpts(&ExtractBudget{}))
		assertErrorKind(t, err, KindUnsupportedContent)
	})

	t.Run("Select(rel+/x) probe refuses link standing in for a selected subdirectory", func(t *testing.T) {
		// Mirror the production welcome template selector: takes only files
		// inside templates/<name>/..., with Roots=["templates"]. For
		// "templates/a", both selectTemplates("templates/a") and
		// onRootPath(["templates"], "templates/a") are false, so the
		// Select(rel+"/x") probe is the sole guard refusing a link there.
		selectTemplates := func(rel string) []string {
			rest, ok := strings.CutPrefix(rel, "templates/")
			if !ok || !strings.Contains(rest, "/") {
				return nil
			}
			return []string{rest}
		}
		makeOpts := func() TarExtractOptions {
			return TarExtractOptions{
				Repo:              "acme/agent-team",
				ExpectedSHA:       sha,
				RequireLeadingPAX: true,
				Select:            selectTemplates,
				Roots:             []string{"templates"},
				SkipEmptyDirs:     true,
				DirMode:           0o700,
				Limits: Limits{
					MaxFiles:     10,
					MaxFileBytes: 1024,
					MaxTotal:     2048,
					MaxUnpacked:  1 << 20,
					OnSymlink:    PolicyReject,
				},
			}
		}

		for _, tc := range []struct {
			name  string
			entry tarTestEntry
		}{
			{
				name:  "symlink at templates/a",
				entry: tarTestEntry{name: "repo-sha/templates/a", typeflag: tar.TypeSymlink, linkname: "/tmp"},
			},
			{
				name:  "hardlink at templates/a",
				entry: tarTestEntry{name: "repo-sha/templates/a", typeflag: tar.TypeLink, linkname: "repo-sha/README.md"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tarData := buildTestTarGz(t, sha, []tarTestEntry{
					{name: "repo-sha/", typeflag: tar.TypeDir},
					{name: "repo-sha/README.md", body: []byte("r")},
					tc.entry,
					{name: "repo-sha/templates/b/f.txt", body: []byte("ok")},
				})
				root, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()

				_, err = ExtractTarGz(bytes.NewReader(tarData), root, makeOpts())
				assertErrorKind(t, err, KindUnsupportedContent)
			})
		}

		t.Run("link outside templates is skipped", func(t *testing.T) {
			tarData := buildTestTarGz(t, sha, []tarTestEntry{
				{name: "repo-sha/", typeflag: tar.TypeDir},
				{name: "repo-sha/outside-link", typeflag: tar.TypeSymlink, linkname: "README.md"},
				{name: "repo-sha/templates/b/f.txt", body: []byte("ok")},
			})
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()

			res, err := ExtractTarGz(bytes.NewReader(tarData), root, makeOpts())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Files != 1 || res.Skipped.Symlinks != 1 {
				t.Fatalf("got files=%d skipped=%+v, want files=1 symlinks=1", res.Files, res.Skipped)
			}
		})
	})

	t.Run("Select combined with SubPath or Tree fails closed", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/skills/tdd/SKILL.md", body: []byte("x")},
		})
		for _, tc := range []struct {
			name    string
			subPath string
			tree    *Tree
		}{
			{name: "Select with SubPath", subPath: "skills/tdd"},
			{name: "Select with Tree", tree: &Tree{TreeSHA: sha}},
			{name: "Select with both", subPath: "skills/tdd", tree: &Tree{TreeSHA: sha}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				root, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()

				opts := baseOpts(&ExtractBudget{})
				opts.SubPath = tc.subPath
				opts.Tree = tc.tree
				_, err = ExtractTarGz(bytes.NewReader(tarData), root, opts)
				assertErrorKind(t, err, KindInvalidSource)
			})
		}
	})

	t.Run("validateDestPath rejects unsafe and oversized destinations", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/", typeflag: tar.TypeDir},
			{name: "repo-sha/a.txt", body: []byte("hello")},
		})
		for _, tc := range []struct {
			name     string
			dest     string
			wantKind Kind
		}{
			{name: "empty dest", dest: "", wantKind: KindUnsupportedContent},
			{name: "absolute dest", dest: "/etc/passwd", wantKind: KindUnsupportedContent},
			{name: "traversal dest", dest: "../escape.txt", wantKind: KindUnsupportedContent},
			{name: "unclean interior dot-dot", dest: "a/../b.txt", wantKind: KindUnsupportedContent},
			{name: "backslash dest", dest: "a\\b.txt", wantKind: KindUnsupportedContent},
			{name: "NUL byte dest", dest: "a\x00b.txt", wantKind: KindUnsupportedContent},
			{name: "exceeds MaxNameBytes", dest: strings.Repeat("a", 40), wantKind: KindTooLarge},
		} {
			t.Run(tc.name, func(t *testing.T) {
				root, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()

				opts := baseOpts(&ExtractBudget{})
				opts.Roots = nil
				opts.Limits.MaxNameBytes = 32
				opts.Select = func(rel string) []string {
					if rel == "a.txt" {
						return []string{tc.dest}
					}
					return nil
				}
				_, err = ExtractTarGz(bytes.NewReader(tarData), root, opts)
				assertErrorKind(t, err, tc.wantKind)
			})
		}
	})

	t.Run("selector mode fails closed when MaxFiles, MaxFileBytes, or MaxTotal is non-positive", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/", typeflag: tar.TypeDir},
			{name: "repo-sha/skills/tdd/SKILL.md", body: []byte("tdd\n")},
		})
		for _, tc := range []struct {
			name   string
			mutate func(*Limits)
		}{
			{name: "MaxFiles zero", mutate: func(l *Limits) { l.MaxFiles = 0 }},
			{name: "MaxFiles negative", mutate: func(l *Limits) { l.MaxFiles = -1 }},
			{name: "MaxFileBytes zero", mutate: func(l *Limits) { l.MaxFileBytes = 0 }},
			{name: "MaxFileBytes negative", mutate: func(l *Limits) { l.MaxFileBytes = -1 }},
			{name: "MaxTotal zero", mutate: func(l *Limits) { l.MaxTotal = 0 }},
			{name: "MaxTotal negative", mutate: func(l *Limits) { l.MaxTotal = -1 }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				root, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()

				opts := baseOpts(&ExtractBudget{})
				tc.mutate(&opts.Limits)
				_, err = ExtractTarGz(bytes.NewReader(tarData), root, opts)
				assertErrorKind(t, err, KindTooLarge)
			})
		}
	})

	t.Run("RequireLeadingPAX fails closed without a full 40-hex ExpectedSHA", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/", typeflag: tar.TypeDir},
			{name: "repo-sha/skills/tdd/SKILL.md", body: []byte("tdd\n")},
		})
		for _, tc := range []struct {
			name        string
			expectedSHA string
		}{
			{name: "empty SHA", expectedSHA: ""},
			{name: "short SHA", expectedSHA: "abcdef1234"},
			{name: "non-hex 40 chars", expectedSHA: strings.Repeat("z", 40)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				root, err := os.OpenRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()

				opts := baseOpts(&ExtractBudget{})
				opts.ExpectedSHA = tc.expectedSHA
				_, err = ExtractTarGz(bytes.NewReader(tarData), root, opts)
				assertErrorKind(t, err, KindInvalidSource)
			})
		}
	})
}
