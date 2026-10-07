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

package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"golang.org/x/text/unicode/norm"
)

// bundlePublishOptions are the flags of a two-step publish.
type bundlePublishOptions struct {
	Title, Key, Note, Entry, Scope string
}

// localFile is one file of a bundle on disk.
type localFile struct {
	rel  string // slash-separated path inside the bundle
	abs  string
	size int64
	sum  string
}

// defaultEntries are the entry files tried, in order, when a folder is
// published without --entry.
var defaultEntries = []string{"index.html", "index.md", "README.md"}

// collectBundle lists the files root publishes as: root itself when it is
// a regular file, or the regular files under it (hidden names skipped,
// symbolic links refused).
func collectBundle(root string) ([]localFile, error) {
	// The root itself may be a symbolic link, as for a single-file publish;
	// links inside a folder are refused.
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		if root, err = filepath.EvalSymlinks(root); err != nil {
			return nil, err
		}
	}
	if info.Mode().IsRegular() {
		return []localFile{{rel: filepath.Base(root), abs: root, size: info.Size()}}, nil
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is neither a regular file nor a folder", root)
	}
	var files []localFile
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; publish its target instead", p)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, localFile{rel: filepath.ToSlash(rel), abs: p, size: fi.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s holds no files to publish", root)
	}
	return files, nil
}

// bundleEntry picks the entry file: the requested one, the only file, or
// the first default entry present.
func bundleEntry(files []localFile, requested string) (string, error) {
	have := make(map[string]bool, len(files))
	for _, f := range files {
		have[f.rel] = true
	}
	if requested != "" {
		e := path.Clean(filepath.ToSlash(requested))
		if !have[e] {
			return "", fmt.Errorf("entry %q is not a file of the bundle", requested)
		}
		return e, nil
	}
	if len(files) == 1 {
		return files[0].rel, nil
	}
	for _, e := range defaultEntries {
		if have[e] {
			return e, nil
		}
	}
	return "", fmt.Errorf("the folder has no %s; name the entry file with --entry", strings.Join(defaultEntries, ", "))
}

func hashArtifactFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// publishBundle publishes root (a file or folder) with the two-step API:
// create a pending version, upload the files the hub asks for, finalize.
func publishBundle(ctx context.Context, svc hubclient.ArtifactService, out, errOut io.Writer, hubEndpoint, root string, opts bundlePublishOptions) error {
	files, err := collectBundle(root)
	if err != nil {
		return err
	}
	entry, err := bundleEntry(files, opts.Entry)
	if err != nil {
		return err
	}
	req := &hubclient.CreateVersionRequest{Title: opts.Title, Key: opts.Key, Scope: opts.Scope, Entry: entry, Note: opts.Note}
	byPath := make(map[string]*localFile, len(files))
	for i := range files {
		f := &files[i]
		if f.sum, err = hashArtifactFile(f.abs); err != nil {
			return err
		}
		byPath[f.rel] = f
		req.Files = append(req.Files, hubclient.ManifestFile{Path: f.rel, Size: f.size, SHA256: f.sum})
	}
	pend, err := svc.CreateVersion(ctx, "", req)
	if err != nil {
		return fmt.Errorf("publish failed: %w%s", err, artifactErrorHint(err, true))
	}
	id, seq := pend.Artifact.ID, pend.Version.Seq
	for _, p := range pend.Upload.Required {
		f, ok := byPath[p]
		if !ok {
			return fmt.Errorf("the hub asked for %q, which is not in the bundle", p)
		}
		if err := uploadLocalFile(ctx, svc, id, seq, f); err != nil {
			return fmt.Errorf("upload %s: %w%s", p, err, artifactErrorHint(err, true))
		}
	}
	resp, err := svc.FinalizeVersion(ctx, id, seq)
	if err != nil {
		return fmt.Errorf("finalize failed: %w%s", err, artifactErrorHint(err, true))
	}
	for _, w := range resp.Warnings {
		_, _ = fmt.Fprintf(errOut, "warning: %s\n", w)
	}
	if opts.Title != "" && resp.Artifact.Title != opts.Title {
		_, _ = fmt.Fprintf(errOut, "note: the artifact keeps its title %q; --title applies when an artifact is created\n", resp.Artifact.Title)
	}
	_, _ = fmt.Fprintf(out, "%s  (v%d)\n", artifacts.FormatRef(id, 0), seq)
	if page := artifactPageURL(hubEndpoint, resp.Artifact.ScopeRef, id); page != "" {
		_, _ = fmt.Fprintln(out, page)
	}
	return nil
}

func uploadLocalFile(ctx context.Context, svc hubclient.ArtifactService, id string, seq int, f *localFile) error {
	fh, err := os.Open(f.abs)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	return svc.UploadFile(ctx, id, seq, f.rel, fh, f.size, f.sum)
}

// bundleFiles drops the files the hub added itself (remote images under
// _remote/), which are not part of what the publisher sent.
func bundleFiles(files []hubclient.ArtifactFile) []hubclient.ArtifactFile {
	out := make([]hubclient.ArtifactFile, 0, len(files))
	for _, f := range files {
		if strings.HasPrefix(f.Path, "_remote/") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// safeBundlePath maps a bundle path to a path under dir, refusing anything
// that would land outside it.
func safeBundlePath(dir, rel string) (string, error) {
	if rel == "" || strings.Contains(rel, "\\") || path.IsAbs(rel) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("refusing unsafe bundle path %q", rel)
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), nil
}

// writeBundle writes every file of version seq under dir, each verified
// against its recorded digest before it is moved into place.
func writeBundle(ctx context.Context, svc hubclient.ArtifactService, stderr io.Writer, id string, seq int, files []hubclient.ArtifactFile, dir string) error {
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		return fmt.Errorf("%s exists and is not a directory; a bundle is written into a directory", dir)
	}
	targets := make([]string, len(files))
	seen := make(map[string]string, len(files))
	for i, f := range files {
		t, err := safeBundlePath(dir, f.Path)
		if err != nil {
			return err
		}
		// On a case-insensitive or normalizing file system these two
		// paths would be one file; refuse rather than overwrite.
		key := strings.ToLower(norm.NFC.String(f.Path))
		if other, ok := seen[key]; ok {
			return fmt.Errorf("bundle paths %q and %q would be the same file on some file systems", other, f.Path)
		}
		seen[key] = f.Path
		targets[i] = t
	}
	for i, f := range files {
		if err := os.MkdirAll(filepath.Dir(targets[i]), 0o755); err != nil {
			return err
		}
		if err := fetchVerified(ctx, svc, id, seq, f, targets[i]); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(stderr, "Wrote %d files to %s\n", len(files), dir)
	return nil
}

func fetchVerified(ctx context.Context, svc hubclient.ArtifactService, id string, seq int, f hubclient.ArtifactFile, target string) error {
	rc, err := svc.OpenFile(ctx, id, seq, f.Path)
	if err != nil {
		return fmt.Errorf("fetch %s: %w%s", f.Path, err, artifactErrorHint(err, false))
	}
	defer func() { _ = rc.Close() }()
	if err := writeVerifiedFile(target, rc, f.SHA256, f.Size); err != nil {
		return fmt.Errorf("%s: %w", f.Path, err)
	}
	return nil
}

// listArtifactVersions prints the versions of the artifact named by ref.
func listArtifactVersions(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, ref string) error {
	id, _, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	meta, err := svc.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get artifact: %w%s", err, artifactErrorHint(err, false))
	}
	versions, err := svc.ListVersions(ctx, id)
	if err != nil {
		return fmt.Errorf("list versions: %w%s", err, artifactErrorHint(err, false))
	}
	if len(versions) == 0 {
		_, _ = fmt.Fprintf(out, "%s has no published version\n", artifacts.FormatRef(id, 0))
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  REF\tKIND\tPUBLISHED\tBY\tFILES\tSIZE\tNOTE")
	for _, v := range versions {
		mark := " "
		if v.Seq == meta.Artifact.CurrentSeq {
			mark = "*"
		}
		by := v.CreatedByKind
		if v.CreatedByRef != "" {
			by += ":" + v.CreatedByRef
		}
		_, _ = fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%d\t%s\t%s\n", mark, artifacts.FormatRef(id, v.Seq), v.Kind,
			v.CreatedAt.UTC().Format("2006-01-02 15:04Z"), by, v.FileCount, humanBytes(v.TotalBytes), noteOneLine(v.Note))
	}
	return tw.Flush()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// noteOneLine keeps a note on one table row.
func noteOneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return s
}
