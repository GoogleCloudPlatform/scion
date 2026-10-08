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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	artifactPublishTitle string
	artifactPublishKey   string
	artifactPublishNote  string
	artifactPublishEntry string
	artifactGetOut       string
	artifactGetForce     bool
)

// artifactCmd is the command group for artifacts.
var artifactCmd = &cobra.Command{
	Use:     "artifact",
	Aliases: []string{"artifacts"},
	Short:   "Publish and fetch artifacts",
	Long: `Publish files and folders as artifacts and fetch them by reference.

An artifact is a published file or folder with a stable reference,
scion://artifact/<id>, that works from any broker and in the web UI. Each
publish under the same --key adds a version; scion://artifact/<id>@<seq>
names one version.
Artifacts require Hub mode and the hub.artifacts experiment.

Commands:
  scion artifact publish <file|dir> [--title] [--key] [--note] [--entry]
  scion artifact get <ref> [--out <path>]      Fetch an artifact
  scion artifact versions <ref>                List an artifact's versions
  scion artifact share <ref> [--ttl 7d]        Create a share link (users only)`,
}

var artifactPublishCmd = &cobra.Command{
	Use:   "publish <file|dir>",
	Short: "Publish a file or folder as an artifact",
	Long: `Publish a file or a folder as an artifact and print its reference.

The artifact is owned by you (or by this agent) and is readable by the
members of the current project. The hub stores the bytes, so readers do not
need access to your filesystem.

A folder is published as a bundle of its regular files (hidden files and
folders, whose names start with ".", are left out; symbolic links inside
the folder are refused). Its entry file, the one the web UI opens, is --entry, or else
index.html, index.md or README.md at the top of the folder.

With --key, publishing again under the same key adds a new version to the
same artifact instead of creating another one. Files unchanged since the
current version are not uploaded again. --note describes the version.

Examples:
  scion artifact publish design.md
  scion artifact publish report.md --title "Q3 report"
  scion artifact publish design.md --key design --note "round 2"
  scion artifact publish ./site --entry index.html --title "Q3 site"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, client, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		return publishArtifactCmd(cmd, settings, client, args[0])
	},
}

// artifactPublishScope is the project a publish names. A hub agent names
// none: the hub homes the artifact in the agent's own project. A user names
// the hub project of the current checkout, never a local-only project id.
func artifactPublishScope(settings *config.Settings) string {
	if config.IsHubManagedAgent() {
		return ""
	}
	return settings.GetHubProjectID()
}

// checkArtifactPublishScope fails a user's publish early, before any
// upload, when the checkout names no hub project to publish into.
func checkArtifactPublishScope(settings *config.Settings) error {
	if !config.IsHubManagedAgent() && artifactPublishScope(settings) == "" {
		return errors.New("this checkout is not linked to a hub project; link it first (scion hub link) to publish artifacts")
	}
	return nil
}

func publishArtifactCmd(cmd *cobra.Command, settings *config.Settings, client hubclient.Client, file string) error {
	if err := checkArtifactPublishScope(settings); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
	defer cancel()
	opts := bundlePublishOptions{
		Title: artifactPublishTitle, Key: artifactPublishKey, Note: artifactPublishNote,
		Entry: artifactPublishEntry, Scope: artifactPublishScope(settings),
	}
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() && opts.Key == "" && opts.Note == "" && opts.Entry == "" {
		return publishArtifact(ctx, client.Artifacts(), cmd.OutOrStdout(), cmd.ErrOrStderr(), GetHubEndpoint(settings), file, opts.Title, opts.Scope)
	}
	return publishBundle(ctx, client.Artifacts(), cmd.OutOrStdout(), cmd.ErrOrStderr(), GetHubEndpoint(settings), file, opts)
}

var artifactGetCmd = &cobra.Command{
	Use:   "get <ref>",
	Short: "Fetch an artifact",
	Long: `Fetch an artifact: its entry file, or with --out a whole bundle.

<ref> is scion://artifact/<id>, scion://artifact/<id>@<seq> for a specific
version, or a bare <id>.

For a single-file artifact the file is written to stdout, or to --out (a
file path, or an existing directory to write the file into under its own
name). For a bundle (several files, or one file inside a folder), the
entry file is written to stdout, or with --out every file of the version
is written under the --out directory (created if needed), keeping its
relative path.

Every file is checked against the size and sha256 recorded at publish
time before it is written; a mismatch writes nothing for that file and
fails. With --out, only plain relative names are written (none starting
with "."), never through a symbolic link below the --out directory, and a
file that already exists is replaced only with --force.

Examples:
  scion artifact get scion://artifact/5f1c2d3e-...
  scion artifact get scion://artifact/5f1c2d3e-...@1 --out ./design.md
  scion artifact get scion://artifact/5f1c2d3e-...@2 --out ./v2/`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
		defer cancel()
		return getArtifact(ctx, client.Artifacts(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], artifactGetOut, artifactGetForce)
	},
}

var artifactVersionsCmd = &cobra.Command{
	Use:   "versions <ref>",
	Short: "List an artifact's versions",
	Long: `List the versions of an artifact, newest first.

Each line shows the version's reference, its kind, when and by whom it
was published, its file count and size, and its note. The current
version is marked with *.

Example:
  scion artifact versions scion://artifact/5f1c2d3e-...`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return listArtifactVersions(ctx, client.Artifacts(), cmd.OutOrStdout(), args[0])
	},
}

func init() {
	artifactPublishCmd.Flags().StringVar(&artifactPublishTitle, "title", "", "Artifact title, set when the artifact is created (default: the entry file name)")
	artifactPublishCmd.Flags().StringVar(&artifactPublishKey, "key", "", "Stable key: publishing again under it adds a version")
	artifactPublishCmd.Flags().StringVar(&artifactPublishNote, "note", "", "Note describing this version")
	artifactPublishCmd.Flags().StringVar(&artifactPublishEntry, "entry", "", "Entry file of a folder, relative to it")
	artifactGetCmd.Flags().StringVarP(&artifactGetOut, "out", "o", "", "Write to this file or directory instead of stdout")
	artifactGetCmd.Flags().BoolVar(&artifactGetForce, "force", false, "Replace files that already exist under --out")
	artifactCmd.AddCommand(artifactPublishCmd, artifactGetCmd, artifactVersionsCmd)
	rootCmd.AddCommand(artifactCmd)
}

// requireArtifactHubClient loads settings and a hub client, failing with an
// artifact-specific message outside Hub mode.
func requireArtifactHubClient() (*config.Settings, hubclient.Client, error) {
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve project path: %w", err)
	}
	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load settings: %w", err)
	}
	if !settings.IsHubEnabled() && !config.IsHubContext() {
		return nil, nil, fmt.Errorf("artifacts require Hub mode. Enable with 'scion hub enable <endpoint>'")
	}
	client, err := getHubClient(settings)
	if err != nil {
		return nil, nil, err
	}
	return settings, client, nil
}

// publishArtifact publishes the file at filePath and prints the reference
// and the artifact's web page.
func publishArtifact(ctx context.Context, svc hubclient.ArtifactService, out, warn io.Writer, hubEndpoint, filePath, title, scope string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (publishing directories is not supported yet)", filePath)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("read %s: %w", filePath, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	resp, err := svc.Publish(ctx, &hubclient.PublishArtifactRequest{
		Name:    filepath.Base(filePath),
		Title:   title,
		Scope:   scope,
		Content: f,
		Size:    info.Size(),
		SHA256:  hex.EncodeToString(h.Sum(nil)),
	})
	if err != nil {
		return fmt.Errorf("publish failed: %w%s", err, artifactErrorHint(err, true))
	}
	a := resp.Artifact
	seq := a.CurrentSeq
	if resp.Version != nil {
		seq = resp.Version.Seq
	}
	_, _ = fmt.Fprintf(out, "%s  (v%d)\n", artifacts.FormatRef(a.ID, 0), seq)
	if page := artifactPageURL(hubEndpoint, a.ScopeRef, a.ID); page != "" {
		_, _ = fmt.Fprintln(out, page)
	}
	for _, w := range resp.Warnings {
		_, _ = fmt.Fprintf(warn, "warning: %s\n", w)
	}
	return nil
}

// artifactPageURL is the artifact's page in the hub web UI.
func artifactPageURL(hubEndpoint, projectID, id string) string {
	if hubEndpoint == "" || projectID == "" {
		return ""
	}
	return strings.TrimRight(hubEndpoint, "/") + "/projects/" + url.PathEscape(projectID) + "/artifacts/" + url.PathEscape(id)
}

// getArtifact fetches the artifact named by ref. A single file, or a
// bundle's entry file when outPath is empty, goes to outPath or stdout; a
// bundle with outPath is written under that directory.
func getArtifact(ctx context.Context, svc hubclient.ArtifactService, stdout, stderr io.Writer, ref, outPath string, replace bool) error {
	id, seq, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	var meta *hubclient.ArtifactResponse
	if seq > 0 {
		meta, err = svc.GetVersion(ctx, id, seq)
	} else {
		meta, err = svc.Get(ctx, id)
	}
	if err != nil {
		return fmt.Errorf("get artifact: %w%s", err, artifactErrorHint(err, false))
	}
	if meta.Version == nil {
		return fmt.Errorf("artifact %s has no published version", id)
	}
	// Pin every read to the version whose manifest was just read, so the
	// digests match even if a new version lands meanwhile.
	seq = meta.Version.Seq
	files := bundleFiles(meta.Version.Files)
	entry := meta.Version.EntryPath
	// A bundle (several files, or one file inside a folder) is written as
	// a tree under --out, keeping relative paths.
	if outPath != "" && (len(files) > 1 || strings.Contains(entry, "/")) {
		return writeBundle(ctx, svc, stderr, id, seq, files, outPath, replace)
	}
	var want *hubclient.ArtifactFile
	for i := range files {
		if files[i].Path == entry {
			want = &files[i]
		}
	}
	if want == nil {
		return errors.New("the hub recorded no digest for " + entry)
	}
	rc, err := svc.OpenFile(ctx, id, seq, entry)
	if err != nil {
		return fmt.Errorf("fetch %s: %w%s", entry, err, artifactErrorHint(err, false))
	}
	defer func() { _ = rc.Close() }()

	if outPath == "" {
		// Spool and verify first, so a consumer reading stdout never sees
		// bytes that fail the check.
		spool, err := os.CreateTemp("", "scion-artifact-*")
		if err != nil {
			return err
		}
		defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
		if err := copyVerified(spool, rc, want.SHA256, want.Size); err != nil {
			return err
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err = io.Copy(stdout, spool)
		return err
	}
	// Into an existing directory, under the entry's own name (checked like
	// a bundle path); otherwise to the file the user named.
	// A path ending in a separator names a directory even if it does not
	// exist yet.
	dir, name := filepath.Dir(outPath), filepath.Base(outPath)
	asDir := strings.HasSuffix(outPath, "/") || strings.HasSuffix(outPath, string(os.PathSeparator))
	if st, err := os.Stat(outPath); asDir || (err == nil && st.IsDir()) {
		dir, name = outPath, path.Base(entry)
		if err := checkBundleName(name); err != nil {
			return err
		}
	}
	out, err := openOutDir(dir)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if err := out.WriteFile(name, replace, func(w io.Writer) error { return copyVerified(w, rc, want.SHA256, want.Size) }); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "Wrote %s\n", filepath.Join(dir, name))
	return nil
}

// artifactErrorHint explains the hub's deliberately uniform answers. A read
// of an artifact the caller may not see is 404, exactly like a missing one,
// so the hint lists the possible causes without telling them apart.
func artifactErrorHint(err error, publishing bool) string {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch {
	case apiErr.StatusCode == http.StatusForbidden && apiErr.Code == artifacts.CodeMissingScope:
		scope := "a required"
		if v, ok := apiErr.Details["scope"].(string); ok && v != "" {
			scope = "the " + v
		}
		return "\nThis agent's credential does not carry " + scope + " scope, usually because the agent was created " +
			"before artifacts were available to it. Recreate the agent so it is issued a current credential."
	case apiErr.StatusCode == http.StatusUnauthorized:
		return "\nThe hub did not accept the credential: sign in again; an agent token may be invalid or expired."
	case apiErr.StatusCode == http.StatusNotFound && !publishing:
		return "\nThe artifact does not exist, or you cannot read it: it is not shared with you or your project, " +
			"or (for an agent) the token lacks the project:artifact:read scope. Artifacts also require the hub.artifacts experiment."
	case apiErr.StatusCode == http.StatusNotFound:
		return "\nThe hub has no artifact service: the hub.artifacts experiment may be off."
	case apiErr.StatusCode == http.StatusForbidden && publishing:
		return "\nYou may not publish artifacts in this project, or may not add versions to this artifact."
	}
	return ""
}

// copyVerified copies src to dst and fails unless the bytes are exactly
// size long and hash to wantDigest. It reads at most size+1 bytes, and
// refuses to copy anything when no digest was recorded.
func copyVerified(dst io.Writer, src io.Reader, wantDigest string, size int64) error {
	if wantDigest == "" {
		return errors.New("the hub recorded no digest for this file")
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, size+1))
	if err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	if n != size {
		return errors.New("artifact content does not match its recorded size")
	}
	if hex.EncodeToString(h.Sum(nil)) != wantDigest {
		return errors.New("artifact content does not match its recorded sha256")
	}
	return nil
}
