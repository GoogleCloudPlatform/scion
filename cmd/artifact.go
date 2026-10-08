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
	"bytes"
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
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/critic"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	artifactPublishTitle  string
	artifactPublishKey    string
	artifactPublishNote   string
	artifactPublishEntry  string
	artifactPublishReview string
	artifactPublishOf     string
	artifactGetOut        string
	artifactGetForce      bool
	artifactGetClean      bool
	artifactGetAccept     bool
	artifactGetKind       string
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
  scion artifact publish <file|dir> --version-of <ref> [--note]
  scion artifact publish <file|dir> --review <ref> [--note]
  scion artifact get <ref> [--out <path>] [--clean|--accept] [--kind publish]
  scion artifact versions <ref>                List an artifact's versions`,
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

With --version-of <ref>, the file or folder is published as the next
version of that artifact (yours, or one you may write), keyed or not. A
single file replaces the current version's entry file, whatever the local
file is named; the bundle's other files are carried over unchanged. A
folder is the whole new bundle.

With --review <ref>, the file or folder is published as a review of that
artifact: a new version of kind review whose text carries CriticMarkup
marks ({>>comment<<}, {~~old~>new~~}, {++insert++}, {--delete--},
{==highlight==}). It becomes the artifact's current version and the
artifact's owner is notified. A review may change nothing outside marks:
the hub rejects it, listing the changed lines, when its text with every
mark rejected differs from the current version. For a plain edit, publish
without --review. A single file reviews the entry file of the current
version; the other files of a bundle are carried over unchanged.

Examples:
  scion artifact publish design.md
  scion artifact publish report.md --title "Q3 report"
  scion artifact publish design.md --key design --note "round 2"
  scion artifact publish ./site --entry index.html --title "Q3 site"
  scion artifact publish plan.md --version-of scion://artifact/5f1c2d3e-...
  scion artifact publish plan.md --review scion://artifact/5f1c2d3e-...`,
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
	if artifactPublishReview != "" && artifactPublishOf != "" {
		return errors.New("--review and --version-of cannot be used together")
	}
	if artifactPublishReview != "" || artifactPublishOf != "" {
		if opts.Title != "" || opts.Key != "" {
			return errors.New("--title and --key do not apply with --review or --version-of, which add a version to the artifact they name")
		}
		if artifactPublishReview != "" {
			return publishReview(ctx, client.Artifacts(), cmd.OutOrStdout(), cmd.ErrOrStderr(), GetHubEndpoint(settings), file, artifactPublishReview, opts)
		}
		return publishVersionOf(ctx, client.Artifacts(), cmd.OutOrStdout(), cmd.ErrOrStderr(), GetHubEndpoint(settings), file, artifactPublishOf, opts)
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

--clean and --accept resolve the CriticMarkup marks of a review in every
text file: --clean rejects them all (the text the reviewer started from),
--accept accepts them all. --kind publish fetches the newest version of
kind publish (at or before <ref>'s version), skipping reviews.

Every file is checked against the size and sha256 recorded at publish
time before it is written (before --clean or --accept is applied); a
mismatch writes nothing for that file and fails. With --out, only plain relative names are written (none starting
with "."), never through a symbolic link below the --out directory, and a
file that already exists is replaced only with --force.

Examples:
  scion artifact get scion://artifact/5f1c2d3e-...
  scion artifact get scion://artifact/5f1c2d3e-...@1 --out ./design.md
  scion artifact get scion://artifact/5f1c2d3e-...@2 --out ./v2/
  scion artifact get scion://artifact/5f1c2d3e-... --clean
  scion artifact get scion://artifact/5f1c2d3e-... --kind publish`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		opts, err := artifactGetOptions(artifactGetClean, artifactGetAccept, artifactGetKind)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
		defer cancel()
		return getArtifactWith(ctx, client.Artifacts(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], artifactGetOut, artifactGetForce, opts)
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
	artifactPublishCmd.Flags().StringVar(&artifactPublishOf, "version-of", "", "Publish as the next version of the artifact with this reference")
	artifactPublishCmd.Flags().StringVar(&artifactPublishReview, "review", "", "Publish as a review (CriticMarkup marks only) of the artifact with this reference")
	artifactGetCmd.Flags().StringVarP(&artifactGetOut, "out", "o", "", "Write to this file or directory instead of stdout")
	artifactGetCmd.Flags().BoolVar(&artifactGetForce, "force", false, "Replace files that already exist under --out")
	artifactGetCmd.Flags().BoolVar(&artifactGetClean, "clean", false, "Reject every CriticMarkup mark in text files (the reviewed baseline)")
	artifactGetCmd.Flags().BoolVar(&artifactGetAccept, "accept", false, "Accept every CriticMarkup mark in text files")
	artifactGetCmd.Flags().StringVar(&artifactGetKind, "kind", "", "Fetch the newest version of this kind (publish or review)")
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

// getOptions are the flags of get that select or transform what is read.
type getOptions struct {
	// Mode is the CriticMarkup projection applied to text files.
	Mode critic.Mode
	// Kind, when set, selects the newest version of that kind at or
	// before the version the reference names.
	Kind string
}

// artifactGetOptions validates get's --clean, --accept and --kind flags.
func artifactGetOptions(clean, accept bool, kind string) (getOptions, error) {
	var o getOptions
	switch {
	case clean && accept:
		return o, errors.New("--clean and --accept cannot be used together")
	case clean:
		o.Mode = critic.Clean
	case accept:
		o.Mode = critic.Accept
	}
	switch kind {
	case "", artifacts.VersionKindPublish, artifacts.VersionKindReview:
		o.Kind = kind
	default:
		return o, fmt.Errorf("--kind must be %s or %s", artifacts.VersionKindPublish, artifacts.VersionKindReview)
	}
	return o, nil
}

// getArtifact fetches the artifact named by ref. A single file, or a
// bundle's entry file when outPath is empty, goes to outPath or stdout; a
// bundle with outPath is written under that directory.
func getArtifact(ctx context.Context, svc hubclient.ArtifactService, stdout, stderr io.Writer, ref, outPath string, replace bool) error {
	return getArtifactWith(ctx, svc, stdout, stderr, ref, outPath, replace, getOptions{})
}

// getArtifactWith is getArtifact with version selection and projection.
// The projection is applied by the CLI after each file is verified against
// its recorded digest, with the same package the hub uses for ?resolve=.
func getArtifactWith(ctx context.Context, svc hubclient.ArtifactService, stdout, stderr io.Writer, ref, outPath string, replace bool, opts getOptions) error {
	id, seq, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	meta, err := artifactVersionFor(ctx, svc, id, seq, opts.Kind)
	if err != nil {
		return err
	}
	if opts.Kind != "" && (seq == 0 || meta.Version.Seq != seq) {
		_, _ = fmt.Fprintf(stderr, "Using %s (%s)\n", artifacts.FormatRef(id, meta.Version.Seq), meta.Version.Kind)
	}
	// Pin every read to the version whose manifest was just read, so the
	// digests match even if a new version lands meanwhile.
	seq = meta.Version.Seq
	files := bundleFiles(meta.Version.Files)
	entry := meta.Version.EntryPath
	// A bundle (several files, or one file inside a folder) is written as
	// a tree under --out, keeping relative paths.
	if outPath != "" && (len(files) > 1 || strings.Contains(entry, "/")) {
		return writeBundle(ctx, svc, stderr, id, seq, files, outPath, replace, opts.Mode)
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
		if err := copyArtifactFile(spool, rc, *want, opts.Mode); err != nil {
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
	if err := out.WriteFile(name, replace, func(w io.Writer) error { return copyArtifactFile(w, rc, *want, opts.Mode) }); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "Wrote %s\n", filepath.Join(dir, name))
	return nil
}

// artifactVersionFor returns the metadata of version seq of artifact id (0
// = current) or, with kind set, of the newest version of that kind at or
// before it.
func artifactVersionFor(ctx context.Context, svc hubclient.ArtifactService, id string, seq int, kind string) (*hubclient.ArtifactResponse, error) {
	var meta *hubclient.ArtifactResponse
	var err error
	if seq > 0 {
		meta, err = svc.GetVersion(ctx, id, seq)
	} else {
		meta, err = svc.Get(ctx, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get artifact: %w%s", err, artifactErrorHint(err, false))
	}
	if meta.Version == nil {
		return nil, fmt.Errorf("artifact %s has no published version", id)
	}
	if kind == "" || meta.Version.Kind == kind {
		return meta, nil
	}
	versions, err := svc.ListVersions(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w%s", err, artifactErrorHint(err, false))
	}
	for _, v := range versions {
		if v.Seq < meta.Version.Seq && v.Kind == kind {
			meta, err = svc.GetVersion(ctx, id, v.Seq)
			if err != nil {
				return nil, fmt.Errorf("get artifact: %w%s", err, artifactErrorHint(err, false))
			}
			return meta, nil
		}
	}
	return nil, fmt.Errorf("artifact %s has no %s version at or before v%d", id, kind, meta.Version.Seq)
}

// copyArtifactFile copies a file of an artifact to dst, verified like copyVerified.
// For a text file and a projection other than Raw, the verified bytes are
// held in memory (at most f.Size) and their projection is written.
func copyArtifactFile(dst io.Writer, src io.Reader, f hubclient.ArtifactFile, mode critic.Mode) error {
	if mode == critic.Raw || !artifacts.IsTextMediaType(f.MediaType) {
		return copyVerified(dst, src, f.SHA256, f.Size)
	}
	var buf bytes.Buffer
	if err := copyVerified(&buf, src, f.SHA256, f.Size); err != nil {
		return err
	}
	_, err := dst.Write(critic.Project(buf.Bytes(), mode))
	return err
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
