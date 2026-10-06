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
	artifactGetOut       string
)

// artifactCmd is the command group for artifacts.
var artifactCmd = &cobra.Command{
	Use:     "artifact",
	Aliases: []string{"artifacts"},
	Short:   "Publish and fetch artifacts",
	Long: `Publish files as artifacts and fetch them by reference.

An artifact is a published file with a stable reference,
scion://artifact/<id>, that works from any broker and in the web UI.
Artifacts require Hub mode and the hub.artifacts experiment.

Commands:
  scion artifact publish <file> [--title <title>]   Publish a file
  scion artifact get <ref> [--out <path>]           Fetch an artifact's file`,
}

var artifactPublishCmd = &cobra.Command{
	Use:   "publish <file>",
	Short: "Publish a file as an artifact",
	Long: `Publish a file as a new artifact and print its reference.

The artifact is owned by you (or by this agent) and is readable by the
members of the current project. The hub stores the bytes, so readers do not
need access to your filesystem.

Examples:
  scion artifact publish design.md
  scion artifact publish report.md --title "Q3 report"`,
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
	return publishArtifact(ctx, client.Artifacts(), cmd.OutOrStdout(), GetHubEndpoint(settings), file, artifactPublishTitle, artifactPublishScope(settings))
}

var artifactGetCmd = &cobra.Command{
	Use:   "get <ref>",
	Short: "Fetch an artifact's file",
	Long: `Fetch the entry file of an artifact.

<ref> is scion://artifact/<id>, scion://artifact/<id>@<seq> for a specific
version, or a bare <id>. The file is written to stdout, or to --out (a file
path, or an existing directory to write the file into under its own name).
When fetching the current version, the bytes are checked against the
sha256 recorded at publish time before anything is written; a mismatch
writes nothing and fails.

Examples:
  scion artifact get scion://artifact/5f1c2d3e-...
  scion artifact get scion://artifact/5f1c2d3e-...@1 --out ./design.md`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
		defer cancel()
		return getArtifact(ctx, client.Artifacts(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], artifactGetOut)
	},
}

func init() {
	artifactPublishCmd.Flags().StringVar(&artifactPublishTitle, "title", "", "Artifact title (default: the file name)")
	artifactGetCmd.Flags().StringVarP(&artifactGetOut, "out", "o", "", "Write to this file or directory instead of stdout")
	artifactCmd.AddCommand(artifactPublishCmd, artifactGetCmd)
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
func publishArtifact(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, hubEndpoint, filePath, title, scope string) error {
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
	return nil
}

// artifactPageURL is the artifact's page in the hub web UI.
func artifactPageURL(hubEndpoint, projectID, id string) string {
	if hubEndpoint == "" || projectID == "" {
		return ""
	}
	return strings.TrimRight(hubEndpoint, "/") + "/projects/" + url.PathEscape(projectID) + "/artifacts/" + url.PathEscape(id)
}

// getArtifact fetches the entry file of the artifact named by ref and
// writes it to outPath, or to stdout when outPath is empty.
func getArtifact(ctx context.Context, svc hubclient.ArtifactService, stdout, stderr io.Writer, ref, outPath string) error {
	id, seq, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	meta, err := svc.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get artifact: %w%s", err, artifactErrorHint(err, false))
	}
	if meta.Version == nil {
		return fmt.Errorf("artifact %s has no published version", id)
	}
	// Single-file artifacts keep one entry path; the current version's
	// manifest names it. Its digest is known only for the current version.
	entry := meta.Version.EntryPath
	wantDigest := ""
	if seq == 0 || seq == meta.Version.Seq {
		for _, f := range meta.Version.Files {
			if f.Path == entry {
				wantDigest = f.SHA256
			}
		}
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
		if err := copyVerified(spool, rc, wantDigest); err != nil {
			return err
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err = io.Copy(stdout, spool)
		return err
	}
	target := outPath
	if st, err := os.Stat(outPath); err == nil && st.IsDir() {
		target = filepath.Join(outPath, path.Base(entry))
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".artifact-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := copyVerified(tmp, rc, wantDigest); err != nil {
		_ = tmp.Close()
		return err
	}
	// CreateTemp makes the file 0600; a fetched document gets the usual
	// mode for a new file.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "Wrote %s\n", target)
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
		return "\nYou may not publish artifacts in this project; for an agent, the token may lack the project:artifact:write scope."
	}
	return ""
}

// copyVerified copies src to dst and, when wantDigest is set, fails if the
// bytes do not hash to it.
func copyVerified(dst io.Writer, src io.Reader, wantDigest string) error {
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	if wantDigest != "" && hex.EncodeToString(h.Sum(nil)) != wantDigest {
		return errors.New("artifact content does not match its recorded sha256")
	}
	return nil
}
