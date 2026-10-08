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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// publishReview publishes root as a review (kind review) of the artifact
// named by ref; see publishToArtifact.
func publishReview(ctx context.Context, svc hubclient.ArtifactService, out, errOut io.Writer, hubEndpoint, root, ref string, opts bundlePublishOptions) error {
	return publishToArtifact(ctx, svc, out, errOut, hubEndpoint, root, ref, artifacts.VersionKindReview, opts)
}

// publishVersionOf publishes root as the next version (kind publish) of the
// artifact named by ref, which the caller owns or may write; see
// publishToArtifact.
func publishVersionOf(ctx context.Context, svc hubclient.ArtifactService, out, errOut io.Writer, hubEndpoint, root, ref string, opts bundlePublishOptions) error {
	return publishToArtifact(ctx, svc, out, errOut, hubEndpoint, root, ref, artifacts.VersionKindPublish, opts)
}

// publishToArtifact adds a version of the given kind to the artifact named
// by ref, through POST /{id}/versions. A single file replaces the current
// version's entry file: it takes the entry's path whatever the local file
// is named, and the version's other files are listed unchanged, so the hub
// needs none of them uploaded. A folder is the whole new bundle; its entry
// defaults to the current version's. A ref naming a version (@<seq>) must
// name the current one.
func publishToArtifact(ctx context.Context, svc hubclient.ArtifactService, out, errOut io.Writer, hubEndpoint, root, ref, kind string, opts bundlePublishOptions) error {
	flag, verb := "--version-of", "publish"
	if kind == artifacts.VersionKindReview {
		flag, verb = "--review", "review"
	}
	id, seq, err := artifacts.ParseRef(ref)
	if err != nil {
		return fmt.Errorf("%s: %w", flag, err)
	}
	meta, err := svc.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get artifact: %w%s", err, artifactErrorHint(err, false))
	}
	if meta.Version == nil {
		return fmt.Errorf("artifact %s has no published version", id)
	}
	current := meta.Version
	if seq > 0 && seq != current.Seq {
		return fmt.Errorf("%s is not the current version (v%d is); %s the current version (scion artifact get %s)",
			artifacts.FormatRef(id, seq), current.Seq, verb, artifacts.FormatRef(id, current.Seq))
	}
	files, err := collectBundle(root)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	req := &hubclient.CreateVersionRequest{Kind: kind, Note: opts.Note}
	if info.Mode().IsRegular() {
		if opts.Entry != "" {
			return errors.New("--entry applies to a folder; a single file replaces the entry file")
		}
		files[0].rel = current.EntryPath
		req.Entry = current.EntryPath
		for _, f := range bundleFiles(current.Files) {
			if f.Path != current.EntryPath {
				req.Files = append(req.Files, hubclient.ManifestFile{Path: f.Path, Size: f.Size, SHA256: f.SHA256})
			}
		}
	} else {
		entry := opts.Entry
		if entry == "" {
			for _, f := range files {
				if f.rel == current.EntryPath {
					entry = f.rel
				}
			}
		}
		if req.Entry, err = bundleEntry(files, entry); err != nil {
			return err
		}
	}
	byPath := make(map[string]*localFile, len(files))
	for i := range files {
		f := &files[i]
		if f.sum, err = hashArtifactFile(f.abs); err != nil {
			return err
		}
		byPath[f.rel] = f
		req.Files = append(req.Files, hubclient.ManifestFile{Path: f.rel, Size: f.size, SHA256: f.sum})
	}
	pend, err := svc.CreateVersion(ctx, id, req)
	if err != nil {
		return fmt.Errorf("%s failed: %w%s", verb, err, artifactErrorHint(err, true))
	}
	if pend.Version == nil {
		return fmt.Errorf("%s failed: the hub's reply has no version", verb)
	}
	if err := uploadRequired(ctx, svc, id, pend.Version.Seq, pend.Upload.Required, byPath); err != nil {
		return err
	}
	var resp *hubclient.ArtifactResponse
	if kind == artifacts.VersionKindReview {
		// The review was made against current: the hub checks it against
		// that version only, and refuses it if another became current.
		resp, err = svc.FinalizeReview(ctx, id, pend.Version.Seq, current.Seq)
	} else {
		resp, err = svc.FinalizeVersion(ctx, id, pend.Version.Seq)
	}
	if err != nil {
		if rejected := unmarkedChangesReport(err); rejected != "" {
			_, _ = fmt.Fprint(errOut, rejected)
			return errors.New("review rejected: it changes text outside CriticMarkup marks, and a review may only add marks. " +
				"Mark every change and publish the review again")
		}
		return fmt.Errorf("%s failed: %w%s", verb, err, artifactErrorHint(err, true))
	}
	for _, w := range resp.Warnings {
		_, _ = fmt.Fprintf(errOut, "warning: %s\n", w)
	}
	if kind == artifacts.VersionKindReview {
		_, _ = fmt.Fprintf(out, "%s  (v%d, review)\n", artifacts.FormatRef(id, 0), pend.Version.Seq)
	} else {
		_, _ = fmt.Fprintf(out, "%s  (v%d)\n", artifacts.FormatRef(id, 0), pend.Version.Seq)
	}
	if page := artifactPageURL(hubEndpoint, resp.Artifact.ScopeRef, id); page != "" {
		_, _ = fmt.Fprintln(out, page)
	}
	return nil
}

// unmarkedChangesReport renders the hub's unmarked_changes rejection, or
// returns "" for any other error. The hub bounds the number of files and
// hunks and the length of each excerpt.
func unmarkedChangesReport(err error) string {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != artifacts.CodeUnmarkedChanges {
		return ""
	}
	var b strings.Builder
	b.WriteString("The review changes text outside CriticMarkup marks (compared with every mark rejected):\n")
	files, _ := apiErr.Details["files"].([]any)
	for _, raw := range files {
		f, _ := raw.(map[string]any)
		path, _ := f["path"].(string)
		change, _ := f["change"].(string)
		fmt.Fprintf(&b, "  %s (%s)\n", path, change)
		hunks, _ := f["hunks"].([]any)
		for _, rh := range hunks {
			h, _ := rh.(map[string]any)
			line, _ := h["line"].(float64)
			fmt.Fprintf(&b, "    line %d:\n", int(line))
			writeHunkSide(&b, "-", h["parent"])
			writeHunkSide(&b, "+", h["clean"])
			if t, _ := h["truncated"].(bool); t {
				b.WriteString("      …\n")
			}
		}
	}
	if t, _ := apiErr.Details["truncated"].(bool); t {
		b.WriteString("  (more files differ)\n")
	}
	return b.String()
}

// writeHunkSide writes one side of a hunk, one prefixed line per line.
func writeHunkSide(b *strings.Builder, prefix string, v any) {
	s, _ := v.(string)
	if s == "" {
		return
	}
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		fmt.Fprintf(b, "      %s %s\n", prefix, l)
	}
}
