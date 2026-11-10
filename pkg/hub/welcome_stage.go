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

package hub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// welcomeSource is the embedded manifest of the welcome project: where its
// templates come from and which repositories its templates' skills may come
// from. Repositories are canonical owner/repo identities.
var welcomeSource = struct {
	Owner, Repo string
	// TemplatesPath is the only folder taken from the agent-team archive.
	TemplatesPath string
	// Coordinator is the template the coordinator agent starts from.
	Coordinator string
	// SkillRepos is the allow-list for skills[].uri repositories.
	SkillRepos []string
}{
	Owner:         "scion-frontiers",
	Repo:          "agent-team",
	TemplatesPath: "templates",
	Coordinator:   "coordinator",
	SkillRepos:    []string{"scion-frontiers/agent-team", "addyosmani/agent-skills", "microsoft/playwright-cli"},
}

// welcomeLimits bounds one welcome install. MaxDownload caps each archive
// body (enforced on the response reader by the fetcher, and checked again
// here); MaxUnpacked caps the decompressed stream read from one archive;
// MaxTotal, MaxFiles and MaxFile cap what is staged, across all archives.
type welcomeLimits struct {
	MaxDownload int64
	MaxUnpacked int64
	MaxTotal    int64
	MaxFiles    int
	MaxFile     int64
}

var defaultWelcomeLimits = welcomeLimits{
	MaxDownload: 64 << 20,
	MaxUnpacked: 512 << 20,
	MaxTotal:    16 << 20,
	MaxFiles:    2000,
	MaxFile:     1 << 20,
}

// welcomeArchiveHosts is the complete set of hosts a welcome download may
// contact, on the first request and on every redirect hop: github.com serves
// the archive URL and redirects to codeload. api.github.com is never used.
var welcomeArchiveHosts = []string{"github.com", "codeload.github.com"}

// welcomeArchiveFetcher downloads one archive body. *remotefetch.Fetcher
// satisfies it.
type welcomeArchiveFetcher interface {
	FetchBytes(ctx context.Context, rawURL string) (*remotefetch.Result, error)
}

// newWelcomeArchiveFetcher returns the production fetcher: https only, the
// hosts in welcomeArchiveHosts on every hop, public addresses only, the body
// capped at lim.MaxDownload while it is read.
func newWelcomeArchiveFetcher(lim welcomeLimits, logger *slog.Logger) *remotefetch.Fetcher {
	return remotefetch.New(remotefetch.Config{
		MaxBytes:     lim.MaxDownload,
		Timeout:      2 * time.Minute,
		MaxRedirects: 3,
		AllowedHosts: welcomeArchiveHosts,
		Logger:       logger,
	})
}

// welcomeError is a welcome failure whose Reason is safe for API responses,
// the ledger and logs. A reason may carry a template or skill name taken from
// the archive, but only one that has passed welcomeSafeName, and the
// allow-listed repository and commit sha; it never holds a local path or any
// other archive member name.
type welcomeError struct {
	Reason string
	err    error
}

func (e *welcomeError) Error() string { return e.Reason }
func (e *welcomeError) Unwrap() error { return e.err }

func welcomeFail(reason string, err error) error { return &welcomeError{Reason: reason, err: err} }

// welcomeReason returns the safe reason for err.
func welcomeReason(err error) string {
	var we *welcomeError
	if errors.As(err, &we) {
		return we.Reason
	}
	return "internal error"
}

var (
	errWelcomePaxMissing  = errors.New("archive has no commit id header")
	errWelcomePaxMismatch = errors.New("archive commit id does not match the requested commit")
	errWelcomeRefused     = errors.New("archive entry refused")
	errWelcomeTooLarge    = errors.New("archive exceeds a size limit")
)

// welcomeBudget counts what has been staged across every archive of one
// install, against the limits.
type welcomeBudget struct {
	lim   welcomeLimits
	files int
	total int64
	// skipped counts link, device and other special entries outside the
	// selected subtrees: never extracted, recorded as a count only.
	skipped int
}

// welcomeStaged is a staged welcome tree: one directory per template in dir,
// with each template's skills vendored into its skills/.
type welcomeStaged struct {
	// dir is the staging directory. It never appears in a response or log.
	dir      string
	manifest welcomeManifest
	// files maps "<template>/<path>" to the file's "sha256:<hex>".
	files map[string]string
}

// welcomeStagingDirName is the hub data dir subdirectory holding staging dirs.
const welcomeStagingDirName = "welcome-staging"

var welcomeStagingNameRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// welcomeStagingBase returns the staging base, <hub data dir>/welcome-staging.
func welcomeStagingBase() (string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, welcomeStagingDirName), nil
}

// newWelcomeStagingDir creates a fresh owner-only staging directory
// <hub data dir>/welcome-staging/<random>.
func newWelcomeStagingDir() (string, error) {
	base, err := welcomeStagingBase()
	if err != nil {
		return "", err
	}
	tree, err := privatefs.OpenTree(base, privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = tree.Close() }()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	name := hex.EncodeToString(b[:])
	if err := tree.MkdirAll(name, privatefs.Private); err != nil {
		return "", err
	}
	return filepath.Join(base, name), nil
}

// removeWelcomeStagingDir removes a staging directory the hub created. It
// removes only a direct <random> child of the staging base, itself inside the
// hub data dir, that is a real directory (not a link), and never follows a
// link inside it. A failure is logged with no path and never reaches the
// user.
func removeWelcomeStagingDir(dir string) {
	if dir == "" {
		return
	}
	if err := removeWelcomeStagingDirChecked(dir); err != nil {
		slog.Warn("welcome project: staging cleanup failed", "reason", welcomeReason(err))
	}
}

// sweepWelcomeStaging removes the staging dirs an earlier hub process left
// behind. Only direct <32-hex> children of the staging base are considered,
// each removed through removeWelcomeStagingDir's checks (a real directory,
// not a link, inside the hub data dir); keep names the dirs of live
// previews, which stay. A missing base is not an error.
func sweepWelcomeStaging(keep map[string]bool) {
	base, err := welcomeStagingBase()
	if err != nil {
		return
	}
	fi, err := os.Lstat(base)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		slog.Warn("welcome project: staging sweep failed", "reason", welcomeReason(welcomeFail("staging base unreadable", err)))
		return
	}
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		if !welcomeStagingNameRe.MatchString(e.Name()) || keep[dir] {
			continue
		}
		removeWelcomeStagingDir(dir)
	}
}

func removeWelcomeStagingDirChecked(dir string) error {
	base, err := welcomeStagingBase()
	if err != nil {
		return welcomeFail("staging base unavailable", err)
	}
	globalDir := filepath.Dir(base)
	if filepath.Dir(dir) != base || !welcomeStagingNameRe.MatchString(filepath.Base(dir)) {
		return welcomeFail("staging path is not a hub staging dir", nil)
	}
	for _, p := range []string{base, dir} {
		fi, err := os.Lstat(p)
		if err != nil {
			return welcomeFail("staging path unavailable", err)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return welcomeFail("staging path is not a directory", nil)
		}
	}
	for _, pair := range [][2]string{{globalDir, base}, {base, dir}} {
		ok, err := pathcanon.Within(pair[0], pair[1])
		if err != nil || !ok {
			return welcomeFail("staging path is outside the hub data dir", err)
		}
	}
	// os.RemoveAll removes a link inside dir as a link; it never follows it.
	if err := os.RemoveAll(dir); err != nil {
		return welcomeFail("staging removal failed", err)
	}
	return nil
}

// welcomeSkillUse is one skills[] entry of one template, resolved.
type welcomeSkillUse struct {
	template string
	name     string // destination directory under the template's skills/
	srcPath  string // folder in the skill repository
	pin      welcomeSkillPin
	owner    string
	repo     string
}

// welcomeResolveAndStage resolves the agent-team HEAD and every skill ref
// with no GitHub API call, downloads one archive per (repo, sha), verifies
// each archive's commit id, extracts within the limits into a fresh staging
// dir, vendors the skills into their templates, and hashes the staged tree.
// On error the staging dir is removed.
func (s *Server) welcomeResolveAndStage(ctx context.Context) (staged *welcomeStaged, err error) {
	rt := s.welcomeRuntime()
	owner, repo, cerr := config.CanonicalGitHubOwnerRepo(welcomeSource.Owner, welcomeSource.Repo)
	if cerr != nil {
		return nil, welcomeFail("invalid template source", cerr)
	}
	teamSHA, rerr := rt.resolveRef(ctx, owner, repo, "")
	if rerr != nil {
		return nil, welcomeFail("could not resolve "+owner+"/"+repo+" HEAD", rerr)
	}
	slog.Info("welcome project: resolved template source", "repo", owner+"/"+repo, "sha", teamSHA)

	dir, derr := newWelcomeStagingDir()
	if derr != nil {
		return nil, welcomeFail("could not create the staging dir", derr)
	}
	defer func() {
		if err != nil {
			removeWelcomeStagingDir(dir)
		}
	}()
	root, oerr := os.OpenRoot(dir)
	if oerr != nil {
		return nil, welcomeFail("could not open the staging dir", oerr)
	}
	defer func() { _ = root.Close() }()

	budget := &welcomeBudget{lim: rt.limits}
	prefix := welcomeSource.TemplatesPath + "/"
	selectTemplates := func(rel string) []string {
		// Only files inside a template directory under templates/ are taken;
		// everything else in the repository (for example .antigravitycli) is
		// not.
		rest, ok := strings.CutPrefix(rel, prefix)
		if !ok || !strings.Contains(rest, "/") {
			return nil
		}
		return []string{rest}
	}
	if err := s.welcomeFetchAndExtract(ctx, owner, repo, teamSHA, root, selectTemplates, []string{welcomeSource.TemplatesPath}, budget); err != nil {
		return nil, err
	}

	templates, terr := welcomeStagedTemplates(root)
	if terr != nil {
		return nil, terr
	}
	if len(templates) == 0 {
		return nil, welcomeFail("the template source has no templates", nil)
	}
	if !slices.Contains(templates, welcomeSource.Coordinator) {
		return nil, welcomeFail("the template source has no "+welcomeSource.Coordinator+" template", nil)
	}

	// Resolve every skill reference before downloading any skill archive.
	var uses []welcomeSkillUse
	for _, tpl := range templates {
		if aerr := welcomeCheckTemplateAuth(root, tpl); aerr != nil {
			return nil, aerr
		}
		refs, rerr := welcomeTemplateSkillRefs(root, tpl)
		if rerr != nil {
			return nil, rerr
		}
		seen := map[string]bool{}
		for _, ref := range refs {
			use, uerr := s.welcomeResolveSkill(ctx, tpl, ref)
			if uerr != nil {
				return nil, uerr
			}
			if seen[use.name] {
				return nil, welcomeFail("template "+tpl+" has two skills named "+use.name, nil)
			}
			seen[use.name] = true
			uses = append(uses, use)
		}
	}

	// One archive per (repo, sha), extracting every skill folder taken from it.
	type archiveKey struct{ owner, repo, sha string }
	byArchive := map[archiveKey][]welcomeSkillUse{}
	var order []archiveKey
	for _, u := range uses {
		k := archiveKey{u.owner, u.repo, u.pin.SHA}
		if _, seen := byArchive[k]; !seen {
			order = append(order, k)
		}
		byArchive[k] = append(byArchive[k], u)
	}
	for _, k := range order {
		group := byArchive[k]
		for _, u := range group {
			// A template's own skills/<name> is never merged with a vendored one.
			if _, err := root.Lstat(filepath.Join(u.template, "skills", u.name)); err == nil {
				return nil, welcomeFail("template "+u.template+" already has a skill named "+u.name, nil)
			}
		}
		sel := func(rel string) []string {
			var dests []string
			for _, u := range group {
				if rest, ok := strings.CutPrefix(rel, u.srcPath+"/"); ok && rest != "" {
					dests = append(dests, u.template+"/skills/"+u.name+"/"+rest)
				}
			}
			return dests
		}
		srcRoots := make([]string, 0, len(group))
		for _, u := range group {
			srcRoots = append(srcRoots, u.srcPath)
		}
		if err := s.welcomeFetchAndExtract(ctx, k.owner, k.repo, k.sha, root, sel, srcRoots, budget); err != nil {
			return nil, err
		}
		for _, u := range group {
			if fi, err := root.Lstat(filepath.Join(u.template, "skills", u.name)); err != nil || !fi.IsDir() {
				return nil, welcomeFail("skill "+u.name+" was not found in "+k.owner+"/"+k.repo+" at "+k.sha, err)
			}
		}
	}

	// Vendoring done: drop skills[] so provisioning resolves nothing remote.
	for _, tpl := range templates {
		if err := welcomeDropSkillRefs(root, tpl); err != nil {
			return nil, err
		}
	}

	// Normalize line endings exactly as the template import will
	// (ResourceStore.Bootstrap normalizes each template dir with
	// transfer.NormalizeRoot), so the manifest hashes the bytes that are
	// stored and the import's own normalization changes nothing.
	if nerr := welcomeNormalizeStaged(dir, templates); nerr != nil {
		return nil, nerr
	}
	// Judge the auth type again on the bytes the broker will read: the
	// skills rewrite re-encodes the config and may reorder its keys.
	for _, tpl := range templates {
		if aerr := welcomeCheckTemplateAuth(root, tpl); aerr != nil {
			return nil, aerr
		}
	}
	files, herr := welcomeHashStaged(dir, templates)
	if herr != nil {
		return nil, herr
	}
	pins := make([]welcomeSkillPin, 0, len(uses))
	for _, u := range uses {
		pins = append(pins, u.pin)
	}
	return &welcomeStaged{
		dir:   dir,
		files: files,
		manifest: welcomeManifest{
			AgentTeamRepo:  owner + "/" + repo,
			AgentTeamSHA:   teamSHA,
			Skills:         pins,
			Templates:      templates,
			FilesSHA256:    welcomeFilesDigest(files),
			SkippedEntries: budget.skipped,
		},
	}, nil
}

// welcomeResolveSkill parses one skills[].uri, checks its repository against
// the allow-list and resolves its own ref to a commit.
func (s *Server) welcomeResolveSkill(ctx context.Context, tpl string, ref welcomeSkillRef) (welcomeSkillUse, error) {
	lower := strings.ToLower(ref.URI)
	httpsForm := strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
	parsed, err := agent.ParseGitHubSkillURI(ref.URI)
	if err != nil {
		return welcomeSkillUse{}, welcomeFail("template "+tpl+" has a skill that is not a GitHub skill URI", err)
	}
	if parsed.TokenSecretName != "" {
		return welcomeSkillUse{}, welcomeFail("template "+tpl+" has a skill that needs a credential", nil)
	}
	owner, repo, err := config.CanonicalGitHubOwnerRepo(parsed.Owner, parsed.Repo)
	if err != nil {
		return welcomeSkillUse{}, welcomeFail("template "+tpl+" has a skill with an invalid repository", err)
	}
	if httpsForm {
		// An https-form skill URI must name its repository as a plain
		// https://github.com/<owner>/<repo>: no userinfo, port, other host,
		// query or fragment (config.CanonicalGitHubRepo), and that identity
		// must equal the parsed one.
		cowner, crepo, cerr := welcomeCanonicalHTTPSRepo(ref.URI)
		if cerr != nil || cowner != owner || crepo != repo {
			return welcomeSkillUse{}, welcomeFail("template "+tpl+" has a skill URL that is not a plain https://github.com repository URL", cerr)
		}
	}
	id := owner + "/" + repo
	if !slices.Contains(welcomeSource.SkillRepos, id) {
		return welcomeSkillUse{}, welcomeFail("skill repository "+id+" is not on the allow-list", nil)
	}
	name := ref.As
	if name == "" {
		name = parsed.SkillName
	}
	if !welcomeSafeName(name) {
		return welcomeSkillUse{}, welcomeFail("template "+tpl+" has a skill with an invalid name", nil)
	}
	src := strings.Trim(path.Clean("/"+parsed.SkillPath), "/")
	if src == "" || src != strings.Trim(parsed.SkillPath, "/") {
		return welcomeSkillUse{}, welcomeFail("template "+tpl+" has a skill with an invalid path", nil)
	}
	// A full commit id is used as is; a branch or tag (or no ref, HEAD) is
	// resolved by ls-remote for that ref.
	sha := strings.ToLower(parsed.Ref)
	if !config.IsFullGitSHA(parsed.Ref) {
		if sha, err = s.welcomeRuntime().resolveRef(ctx, owner, repo, parsed.Ref); err != nil {
			return welcomeSkillUse{}, welcomeFail("could not resolve skill "+name+" in "+id, err)
		}
	}
	slog.Info("welcome project: resolved skill", "repo", id, "ref", parsed.Ref, "sha", sha)
	return welcomeSkillUse{
		template: tpl,
		name:     name,
		srcPath:  src,
		pin:      welcomeSkillPin{Repo: id, Ref: parsed.Ref, SHA: sha},
		owner:    owner,
		repo:     repo,
	}, nil
}

// welcomeCanonicalHTTPSRepo canonicalizes the repository of an https-form
// skill URI: the URI with its path cut to the first two segments goes
// through config.CanonicalGitHubRepo, so its scheme, host, userinfo, port,
// query and fragment are all checked.
func welcomeCanonicalHTTPSRepo(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(segs) < 2 {
		return "", "", errors.New("skill URL names no repository")
	}
	u2 := *u
	u2.Path, u2.RawPath = "/"+segs[0]+"/"+segs[1], ""
	return config.CanonicalGitHubRepo(u2.String())
}

var welcomeSafeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func welcomeSafeName(name string) bool {
	return welcomeSafeNameRe.MatchString(name) && !strings.Contains(name, "..")
}

// welcomeFetchAndExtract downloads github.com/<owner>/<repo> at sha and
// extracts the entries sel maps into root. roots are the archive-relative
// directories the selection starts from (see extractWelcomeArchive).
func (s *Server) welcomeFetchAndExtract(ctx context.Context, owner, repo, sha string, root *os.Root, sel func(string) []string, roots []string, budget *welcomeBudget) error {
	id := owner + "/" + repo
	archiveURL, err := config.GitHubArchiveURLBySHA(owner, repo, sha)
	if err != nil {
		return welcomeFail("invalid archive for "+id, err)
	}
	res, err := s.welcomeRuntime().fetcher.FetchBytes(ctx, archiveURL)
	if err != nil {
		var fe *remotefetch.Error
		if errors.As(err, &fe) && fe.Reason == remotefetch.ReasonTooLarge {
			return welcomeFail("the archive of "+id+" at "+sha+" exceeds the download limit", err)
		}
		return welcomeFail("could not download "+id+" at "+sha, err)
	}
	if res == nil || int64(len(res.Body)) > budget.lim.MaxDownload {
		return welcomeFail("the archive of "+id+" at "+sha+" exceeds the download limit", errWelcomeTooLarge)
	}
	before := budget.skipped
	if err := extractWelcomeArchive(res.Body, sha, root, sel, roots, budget); err != nil {
		reason := "the archive of " + id + " at " + sha + " was refused"
		switch {
		case errors.Is(err, errWelcomePaxMissing):
			reason = "the archive of " + id + " at " + sha + " has no commit id"
		case errors.Is(err, errWelcomePaxMismatch):
			reason = "the archive of " + id + " is not commit " + sha
		case errors.Is(err, errWelcomeTooLarge):
			reason = "the archive of " + id + " at " + sha + " exceeds a size limit"
		}
		slog.Warn("welcome project: archive refused", "repo", id, "sha", sha, "reason", reason)
		return welcomeFail(reason, err)
	}
	if n := budget.skipped - before; n > 0 {
		slog.Debug("welcome project: special entries outside the selected subtrees skipped", "repo", id, "sha", sha, "count", n)
	}
	return nil
}

// extractWelcomeArchive extracts a GitHub archive (a gzip tarball with one
// top-level directory) into root, strictly:
//
//   - the first entry must be the pax global header whose comment (the
//     commit id git-archive writes) equals sha, or nothing is written;
//   - every entry name must be relative and free of ".." components, and all
//     entries must share the single top-level directory;
//   - an entry on a selected path must be a regular file or a directory: a
//     link, device, FIFO or any other type there is refused. A selected path
//     is one sel selects, one at or below a selected directory, a selection
//     root itself (templates/, a skill's source directory) or any ancestor
//     directory of a root, so a link cannot stand in for the selection or
//     for a directory on the way to it. The same entry anywhere else is
//     skipped, never extracted, and counted in budget.skipped;
//   - the limits are counted as entries are read: the decompressed stream,
//     each file's size, the staged file count and total bytes.
//
// Any breach fails the whole extraction. sel maps an archive-relative path
// (top-level directory removed) to the root-relative destinations it is
// written to, none when the entry is not taken. Files are created with
// O_EXCL through root, so nothing is overwritten and no link is followed.
// roots are the archive-relative selection roots (no trailing slash).
func extractWelcomeArchive(body []byte, sha string, root *os.Root, sel func(string) []string, roots []string, budget *welcomeBudget) error {
	gzr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: not a gzip archive", errWelcomeRefused)
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(&cappedReader{r: gzr, remaining: budget.lim.MaxUnpacked})

	next := func() (*tar.Header, error) {
		hdr, err := tr.Next()
		if err != nil && err != io.EOF {
			if errors.Is(err, errTemplateSourceTooLarge) {
				return nil, errWelcomeTooLarge
			}
			return nil, fmt.Errorf("%w: invalid tar stream", errWelcomeRefused)
		}
		return hdr, err
	}

	first, err := next()
	if err == io.EOF {
		return errWelcomePaxMissing
	}
	if err != nil {
		return err
	}
	if first.Typeflag != tar.TypeXGlobalHeader {
		return errWelcomePaxMissing
	}
	comment, ok := first.PAXRecords["comment"]
	if !ok || strings.TrimSpace(comment) == "" {
		return errWelcomePaxMissing
	}
	if !strings.EqualFold(strings.TrimSpace(comment), sha) {
		return errWelcomePaxMismatch
	}

	top := ""
	for {
		hdr, err := next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			return fmt.Errorf("%w: second global header", errWelcomeRefused)
		}
		name := hdr.Name
		if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\x00\\") {
			return fmt.Errorf("%w: unsafe name", errWelcomeRefused)
		}
		trimmed := strings.TrimSuffix(name, "/")
		for _, c := range strings.Split(trimmed, "/") {
			if c == ".." || c == "." {
				return fmt.Errorf("%w: unsafe name", errWelcomeRefused)
			}
		}
		head, rel, _ := strings.Cut(trimmed, "/")
		if top == "" {
			top = head
		} else if head != top {
			return fmt.Errorf("%w: more than one top-level directory", errWelcomeRefused)
		}
		if rel == "" {
			if hdr.Typeflag != tar.TypeDir {
				return fmt.Errorf("%w: top-level entry is not a directory", errWelcomeRefused)
			}
			continue
		}
		dests := sel(rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is read as a regular file
		default:
			if len(dests) > 0 || welcomeSelectsBelow(sel, rel) || welcomeOnRootPath(roots, rel) {
				return fmt.Errorf("%w: link or special file", errWelcomeRefused)
			}
			// Outside the selected subtrees: skipped, never extracted
			// (release-manager 21d9afa0), and counted without its name.
			budget.skipped++
			continue
		}
		if len(dests) == 0 {
			continue
		}
		if hdr.Size < 0 || hdr.Size > budget.lim.MaxFile {
			return fmt.Errorf("%w: file over the per-file limit", errWelcomeTooLarge)
		}
		budget.files += len(dests)
		if budget.files > budget.lim.MaxFiles {
			return fmt.Errorf("%w: too many files", errWelcomeTooLarge)
		}
		budget.total += hdr.Size * int64(len(dests))
		if budget.total > budget.lim.MaxTotal {
			return fmt.Errorf("%w: total size over the limit", errWelcomeTooLarge)
		}
		data, err := io.ReadAll(io.LimitReader(tr, budget.lim.MaxFile+1))
		if err != nil {
			if errors.Is(err, errTemplateSourceTooLarge) {
				return errWelcomeTooLarge
			}
			return fmt.Errorf("%w: invalid tar stream", errWelcomeRefused)
		}
		if int64(len(data)) > budget.lim.MaxFile {
			return fmt.Errorf("%w: file over the per-file limit", errWelcomeTooLarge)
		}
		mode := extractFileMode(hdr.Mode)
		for _, dest := range dests {
			if err := welcomeWriteFile(root, dest, data, mode); err != nil {
				return err
			}
		}
	}
}

// welcomeSelectsBelow reports whether sel takes anything at or below rel
// treated as a directory, so a link or special entry standing in for a
// selected directory is refused too.
func welcomeSelectsBelow(sel func(string) []string, rel string) bool {
	return len(sel(rel+"/x")) > 0
}

// welcomeOnRootPath reports whether rel is a selection root or an ancestor
// directory of one, the directories every selected entry is reached through.
func welcomeOnRootPath(roots []string, rel string) bool {
	for _, r := range roots {
		if rel == r || strings.HasPrefix(r, rel+"/") {
			return true
		}
	}
	return false
}

// welcomeWriteFile writes data to the root-relative slash path dest,
// creating parents owner-only. It refuses a name that would leave root and
// never replaces an existing file.
func welcomeWriteFile(root *os.Root, dest string, data []byte, mode os.FileMode) error {
	clean := path.Clean(dest)
	if clean != dest || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("%w: unsafe destination", errWelcomeRefused)
	}
	local := filepath.FromSlash(clean)
	if dir := filepath.Dir(local); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("%w: cannot create a directory", errWelcomeRefused)
		}
	}
	f, err := root.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("%w: cannot create a file", errWelcomeRefused)
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return fmt.Errorf("%w: cannot write a file", errWelcomeRefused)
	}
	return nil
}

// welcomeStagedTemplates lists the template directories staged in root.
func welcomeStagedTemplates(root *os.Root) ([]string, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, welcomeFail("could not read the staging dir", err)
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, welcomeFail("could not read the staging dir", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			return nil, welcomeFail("the staged tree has an unexpected entry", nil)
		}
		if !welcomeSafeName(e.Name()) {
			return nil, welcomeFail("the template source has a template with an invalid name", nil)
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// welcomeSkillRef is the part of a skills[] entry the welcome flow reads.
type welcomeSkillRef struct {
	URI string `yaml:"uri" json:"uri"`
	As  string `yaml:"as" json:"as"`
}

// welcomeAgentConfigFile returns the template's agent config file name in
// root, "" when it has none.
// A template that ships more than one agent config is refused, since which
// one applies would be ambiguous.
func welcomeAgentConfigFile(root *os.Root, tpl string) (string, error) {
	found := ""
	for _, name := range []string{"scion-agent.yaml", "scion-agent.yml", "scion-agent.json"} {
		p := filepath.Join(tpl, name)
		fi, err := root.Lstat(p)
		if errors.Is(err, iofs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", welcomeFail("could not read template "+tpl, err)
		}
		if !fi.Mode().IsRegular() {
			return "", welcomeFail("template "+tpl+" has an invalid agent config", nil)
		}
		if found != "" {
			return "", welcomeFail("template "+tpl+" has more than one agent config", nil)
		}
		found = p
	}
	return found, nil
}

// welcomeAllowedAuthTypes is the agent schema's auth_selectedType enum
// (pkg/config/schemas/agent-v1.schema.json).
var welcomeAllowedAuthTypes = map[string]bool{"api-key": true, "oauth-token": true, "auth-file": true, "vertex-ai": true}

// welcomeCheckTemplateAuth refuses a staged template whose agent config sets
// an auth type outside the agent schema's enum, so the no-auth sentinel
// "none" (or any other value) cannot reach the coordinator create: the hub's
// no-auth guard sees only the request and the auto fallback, while the broker
// honours a template's auth_selectedType (sec F-3). The config is decoded
// with the broker's own decoder (config.ParseScionAgentConfig, as
// Template.LoadConfig decodes it), so every spelling the broker maps to
// AuthSelectedType is judged by its decoded value. The schema validator is a
// second layer: only its findings on auth_selectedType refuse, so a config
// that is otherwise accepted today is not newly refused here. Staging runs
// this check again on the final bytes, after the skills rewrite and the
// line-ending normalization, so neither can change the verdict.
func welcomeCheckTemplateAuth(root *os.Root, tpl string) error {
	p, err := welcomeAgentConfigFile(root, tpl)
	if err != nil || p == "" {
		return err
	}
	data, err := root.ReadFile(p)
	if err != nil {
		return welcomeFail("could not read template "+tpl, err)
	}
	cfg, err := config.ParseScionAgentConfig(p, data)
	if err != nil {
		return welcomeFail("template "+tpl+" has an unreadable agent config", err)
	}
	if cfg.AuthSelectedType != "" && !welcomeAllowedAuthTypes[cfg.AuthSelectedType] {
		return welcomeFail("template "+tpl+" sets an auth type that is not allowed", nil)
	}
	findings, err := config.ValidateAgentConfig(data, "1")
	if err != nil {
		return welcomeFail("template "+tpl+" has an unreadable agent config", err)
	}
	for _, f := range findings {
		if f.Path == "auth_selectedType" || strings.HasPrefix(f.Path, "auth_selectedType/") {
			return welcomeFail("template "+tpl+" sets an auth type that is not allowed", nil)
		}
	}
	return nil
}

// welcomeTemplateSkillRefs reads a staged template's skills[] entries.
func welcomeTemplateSkillRefs(root *os.Root, tpl string) ([]welcomeSkillRef, error) {
	p, err := welcomeAgentConfigFile(root, tpl)
	if err != nil || p == "" {
		return nil, err
	}
	data, err := root.ReadFile(p)
	if err != nil {
		return nil, welcomeFail("could not read template "+tpl, err)
	}
	var doc struct {
		Skills []welcomeSkillRef `yaml:"skills" json:"skills"`
	}
	if strings.HasSuffix(p, ".json") {
		err = json.Unmarshal(data, &doc)
	} else {
		err = yaml.Unmarshal(data, &doc)
	}
	if err != nil {
		return nil, welcomeFail("template "+tpl+" has an unreadable agent config", err)
	}
	return doc.Skills, nil
}

// welcomeDropSkillRefs removes the skills key from a staged template's agent
// config, after its skills were vendored into skills/.
func welcomeDropSkillRefs(root *os.Root, tpl string) error {
	p, err := welcomeAgentConfigFile(root, tpl)
	if err != nil || p == "" {
		return err
	}
	data, err := root.ReadFile(p)
	if err != nil {
		return welcomeFail("could not read template "+tpl, err)
	}
	var out []byte
	if strings.HasSuffix(p, ".json") {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			return welcomeFail("template "+tpl+" has an unreadable agent config", err)
		}
		if _, ok := m["skills"]; !ok {
			return nil
		}
		delete(m, "skills")
		if out, err = json.MarshalIndent(m, "", "  "); err != nil {
			return welcomeFail("could not rewrite template "+tpl, err)
		}
	} else {
		var node yaml.Node
		if err := yaml.Unmarshal(data, &node); err != nil {
			return welcomeFail("template "+tpl+" has an unreadable agent config", err)
		}
		if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
			return nil
		}
		m := node.Content[0]
		removed := false
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == "skills" {
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
				removed = true
				break
			}
		}
		if !removed {
			return nil
		}
		if out, err = yaml.Marshal(&node); err != nil {
			return welcomeFail("could not rewrite template "+tpl, err)
		}
	}
	if err := root.Remove(p); err != nil {
		return welcomeFail("could not rewrite template "+tpl, err)
	}
	if err := welcomeWriteFile(root, filepath.ToSlash(p), out, 0o644); err != nil {
		return welcomeFail("could not rewrite template "+tpl, err)
	}
	return nil
}

// welcomeNormalizeStaged normalizes each staged template directory in place
// with transfer.NormalizeRoot, through a root opened at that directory, the
// same call ResourceStore.Bootstrap makes on import.
func welcomeNormalizeStaged(dir string, templates []string) error {
	for _, tpl := range templates {
		root, err := os.OpenRoot(filepath.Join(dir, tpl))
		if err != nil {
			return welcomeFail("could not normalize the staged tree", err)
		}
		nerr := transfer.NormalizeRoot(root)
		_ = root.Close()
		if nerr != nil {
			return welcomeFail("could not normalize the staged tree", nerr)
		}
	}
	return nil
}

// welcomeHashStaged hashes every staged file with sha256, collecting each
// template directory the way template import does, and returns
// "<template>/<path>" -> "sha256:<hex>". A non-regular file anywhere fails.
func welcomeHashStaged(dir string, templates []string) (map[string]string, error) {
	// Collection skips links and special files, so check for them first.
	if err := welcomeCheckRegularTree(dir); err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, tpl := range templates {
		tplDir := filepath.Join(dir, tpl)
		root, err := os.OpenRoot(tplDir)
		if err != nil {
			return nil, welcomeFail("could not hash the staged tree", err)
		}
		collected, err := transfer.CollectFilesFromRoot(root, tplDir, nil)
		_ = root.Close()
		if err != nil {
			return nil, welcomeFail("could not hash the staged tree", err)
		}
		for _, fi := range collected {
			files[tpl+"/"+fi.Path] = fi.Hash
		}
	}
	return files, nil
}

// welcomeCheckRegularTree fails if dir holds anything other than directories
// and regular files. It does not follow links.
func welcomeCheckRegularTree(dir string) error {
	err := filepath.WalkDir(dir, func(_ string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type().IsRegular() {
			return nil
		}
		return errWelcomeRefused
	})
	if err != nil {
		return welcomeFail("the staged tree has a non-regular file", err)
	}
	return nil
}

// welcomeFilesDigest is the aggregate sha256 over a file map: the sorted
// "<path>\x00<hash>\n" lines.
func welcomeFilesDigest(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		_, _ = io.WriteString(h, p+"\x00"+files[p]+"\n")
	}
	return transfer.HashPrefix + hex.EncodeToString(h.Sum(nil))
}
