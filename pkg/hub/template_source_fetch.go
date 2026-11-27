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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// templateSourceHosts is retained for import_source_guard.go until Phase 2
// migrates general GitHub imports onto pkg/githubfetch.
var templateSourceHosts = []string{"github.com", "api.github.com", "codeload.github.com"}

// errTemplateSourceTooLarge, cappedReader and extractFileMode are retained for
// welcome_stage.go until Phase 7 migrates welcome archive extraction.
var errTemplateSourceTooLarge = errors.New("template source exceeds the size limit")

type cappedReader struct {
	r         io.Reader
	remaining int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.remaining < 0 {
		return 0, errTemplateSourceTooLarge
	}
	if int64(len(p)) > c.remaining+1 {
		p = p[:c.remaining+1]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	if c.remaining < 0 {
		return n, errTemplateSourceTooLarge
	}
	return n, err
}

func extractFileMode(archiveMode int64) os.FileMode {
	if archiveMode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

var sourceCommitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// validateSourceCommit returns raw trimmed if it is a valid 40-character
// lowercase hex git commit SHA; otherwise it returns "" and logs at debug
// level when raw is non-empty.
func validateSourceCommit(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if !sourceCommitRe.MatchString(trimmed) {
		slog.Debug("ignoring invalid template source commit", "raw", trimmed)
		return ""
	}
	return trimmed
}

// errTemplateSourceUnsupported is returned for a source URL reimport does not
// support. Its message is fixed text and never includes the URL.
var errTemplateSourceUnsupported = errors.New(
	"refreshing from source is only supported for templates imported from a GitHub folder URL (https://github.com/<owner>/<repo>/tree/<branch>/<path>)")

// reimportFetchError carries a classified githubfetch failure with a
// caller-safe fixed message and optional Retry-After duration for 429s.
type reimportFetchError struct {
	Kind       githubfetch.Kind
	RetryAfter time.Duration
	Message    string
}

func (e *reimportFetchError) Error() string { return e.Message }

// mapTemplateReimportError maps a githubfetch error into a caller-safe fixed
// message. Raw error details, tokens, and redirect URLs stay in server logs.
func mapTemplateReimportError(err error) error {
	var ghe *githubfetch.Error
	if !errors.As(err, &ghe) {
		return &reimportFetchError{
			Kind:    githubfetch.KindUnreachable,
			Message: "could not download the template source",
		}
	}
	switch ghe.Kind {
	case githubfetch.KindNotFound:
		msg := "GitHub repository, ref, or folder was not found, or is private and no repository credential is configured"
		if ghe.CredKind != githubfetch.CredAnonymous {
			msg = "GitHub repository, ref, or folder was not found, or the template scope's repository credential cannot read it"
		}
		if ghe.Hint != "" {
			msg += " (" + ghe.Hint + ")"
		}
		return &reimportFetchError{Kind: ghe.Kind, Message: msg}
	case githubfetch.KindAuthRejected:
		msg := "GitHub rejected the template scope's repository credential (it may be invalid or expired)"
		if ghe.Hint != "" {
			msg += " (" + ghe.Hint + ")"
		}
		return &reimportFetchError{Kind: ghe.Kind, Message: msg}
	case githubfetch.KindForbidden:
		msg := "GitHub denied access to the template repository"
		if ghe.Hint != "" {
			msg += " (" + ghe.Hint + ")"
		}
		return &reimportFetchError{Kind: ghe.Kind, Message: msg}
	case githubfetch.KindRateLimited:
		return &reimportFetchError{
			Kind:       ghe.Kind,
			RetryAfter: ghe.RetryAfter,
			Message:    "GitHub rate limit exceeded; retry later",
		}
	case githubfetch.KindTooLarge:
		return &reimportFetchError{
			Kind:    ghe.Kind,
			Message: "template source exceeds the size or file count limit",
		}
	case githubfetch.KindTimeout:
		return &reimportFetchError{
			Kind:    ghe.Kind,
			Message: "timed out downloading the template source",
		}
	case githubfetch.KindUnsupportedContent:
		return &reimportFetchError{
			Kind:    ghe.Kind,
			Message: "template source contains unsupported content",
		}
	case githubfetch.KindUpstream:
		return &reimportFetchError{
			Kind:    ghe.Kind,
			Message: "GitHub returned an invalid or inconsistent archive",
		}
	default:
		return &reimportFetchError{
			Kind:    ghe.Kind,
			Message: "could not download the template source",
		}
	}
}

func (s *Server) templateReimportLimits() githubfetch.Limits {
	if s.workstation {
		return githubfetch.ImportLocalLimits()
	}
	return githubfetch.ImportHostedLimits()
}

func (s *Server) templateReimportNetworkMode() remotefetch.NetworkMode {
	if s.workstation {
		return remotefetch.NetworkSystem
	}
	return remotefetch.NetworkVetted
}

func (s *Server) getTemplateGitHubClient(lim githubfetch.Limits) *githubfetch.Client {
	if s.templateGitHubClient != nil {
		return s.templateGitHubClient
	}
	ep := githubfetch.DefaultEndpoint()
	doer := s.newTemplateGitHubDoer(ep, lim)
	return githubfetch.New(githubfetch.Options{
		Endpoint:   ep,
		Doer:       doer,
		Cooldown:   s.githubCooldown(),
		MaxRetries: 1,
	})
}

func (s *Server) newTemplateGitHubDoer(ep githubfetch.Endpoint, lim githubfetch.Limits) githubfetch.Doer {
	network := s.templateReimportNetworkMode()
	reqTimeout := lim.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = lim.Timeout
	}
	return githubfetch.NewDoer(ep, network, lim.MaxDownload, reqTimeout, s.resourceLog)
}

// targetTemplatePersistence is the template persistence used by reimport. It
// always resolves the record to update by the target template's ID, never by
// the slug of the source folder name, it never creates a template, and it
// does not import bundled harness-configs: a refresh writes only into the
// template it was asked to refresh.
type targetTemplatePersistence struct {
	*templatePersistence
	id string
}

func (p *targetTemplatePersistence) GetBySlug(ctx context.Context, _, scope, scopeID string) (*ResourceRecord, error) {
	t, err := p.s.store.GetTemplate(ctx, p.id)
	if err != nil {
		return nil, fmt.Errorf("template to refresh could not be loaded: %w", err)
	}
	if t.Scope != scope || t.ScopeID != scopeID {
		return nil, errors.New("template scope changed during refresh")
	}
	p.model = t
	return templateToRecord(t), nil
}

func (p *targetTemplatePersistence) Create(context.Context, *ResourceRecord, *collectedDir) error {
	return errors.New("refreshing a template never creates a new template")
}

// PostFinalize does nothing: a refresh writes only the target template, so
// harness-configs bundled in the source (a harness-configs/ folder) are not
// created or updated.
func (p *targetTemplatePersistence) PostFinalize(context.Context, *ResourceRecord, *collectedDir) {}

// OnHashMatch does nothing for the same reason.
func (p *targetTemplatePersistence) OnHashMatch(context.Context, *ResourceRecord, *collectedDir) (bool, error) {
	return false, nil
}

// targetTemplateStore returns a ResourceStore that writes only into the
// template with the given ID.
func (s *Server) targetTemplateStore(id string) *ResourceStore {
	return &ResourceStore{
		srv:   s,
		pers:  &targetTemplatePersistence{templatePersistence: &templatePersistence{s: s}, id: id},
		hubID: s.HubID(),
	}
}

// selectTemplateSourceDir picks the one discovered folder that corresponds to
// tmpl: a folder named after the template's name or slug, or whose name
// slugifies to the template's slug. It fails when none or several match.
func selectTemplateSourceDir(dirs []resourceDir, tmpl *store.Template) (resourceDir, error) {
	var matches []resourceDir
	for _, d := range dirs {
		if d.name == tmpl.Name || d.name == tmpl.Slug || (tmpl.Slug != "" && api.Slugify(d.name) == tmpl.Slug) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return resourceDir{}, errors.New("the source does not contain this template")
	case 1:
		return matches[0], nil
	default:
		return resourceDir{}, errors.New("the source contains more than one folder matching this template")
	}
}

// reimportTemplateSource resolves src, checks whether the upstream folder tree
// SHA is unchanged (skipping download and storage write when it is), and
// otherwise streams and extracts the folder via pkg/githubfetch and refreshes
// tmpl. sourceURL preserves the original ref (branch, tag, SHA, or slashed
// branch); source_commit records the resolved 40-hex commit SHA.
func (s *Server) reimportTemplateSource(ctx context.Context, src githubfetch.Source, sourceURL string, tmpl *store.Template, progress importProgressFunc) ([]string, error) {
	return s.reimportTemplateSourceWithLimits(ctx, src, sourceURL, tmpl, progress, s.templateReimportLimits())
}

func (s *Server) reimportTemplateSourceWithLimits(ctx context.Context, src githubfetch.Source, sourceURL string, tmpl *store.Template, progress importProgressFunc, lim githubfetch.Limits) ([]string, error) {
	kind := s.templateImportKind()
	if s.GetStorage() == nil {
		return nil, fmt.Errorf("%s storage is not configured", kind.noun)
	}

	cred := s.githubCredential(ctx, ghCredRequest{
		Scope:   tmpl.Scope,
		ScopeID: tmpl.ScopeID,
		Owner:   src.Owner,
		Repo:    src.Repo,
	})
	client := s.getTemplateGitHubClient(lim)

	resolved, resolvedSrc, err := client.ResolveRef(ctx, src, cred)
	if err != nil {
		s.resourceLog.Warn("template reimport: ref resolution failed",
			"owner", src.Owner, "repo", src.Repo, "error", err)
		return nil, mapTemplateReimportError(err)
	}

	tree, err := client.ListTree(ctx, resolvedSrc, resolved.CommitSHA, cred, lim)
	if err != nil {
		s.resourceLog.Warn("template reimport: tree listing failed",
			"owner", src.Owner, "repo", src.Repo, "commit", resolved.CommitSHA, "error", err)
		return nil, mapTemplateReimportError(err)
	}

	// Skip tarball download and storage write when the template already has
	// stored content for this exact sourceURL and the upstream folder tree SHA
	// is unchanged (same commit, or a newer commit with an identical tree SHA).
	prevCommit := validateSourceCommit(tmpl.SourceCommit)
	if prevCommit != "" && tmpl.SourceURL == sourceURL && tmpl.ContentHash != "" {
		unchanged := resolved.CommitSHA == prevCommit
		if !unchanged && tree.TreeSHA != "" {
			if prevTree, prevErr := client.ListTree(ctx, resolvedSrc, prevCommit, cred, lim); prevErr == nil && prevTree.TreeSHA != "" && prevTree.TreeSHA == tree.TreeSHA {
				unchanged = true
			}
		}
		if unchanged {
			if resolved.CommitSHA != prevCommit {
				precond := store.TemplateContentPrecondition{
					ContentHash: tmpl.ContentHash,
					Layout:      tmpl.Layout,
				}
				tmpl.SourceCommit = resolved.CommitSHA
				if err := s.store.UpdateTemplateContent(ctx, tmpl, precond); err != nil {
					return nil, err
				}
			}
			emit(progress, ResourceImportEvent{Type: ImportEventDiscovered, Total: 1, Names: []string{tmpl.Name}})
			emit(progress, ResourceImportEvent{Type: ImportEventStarted, Name: tmpl.Name})
			emit(progress, ResourceImportEvent{Type: ImportEventCompleted, Name: tmpl.Name, Completed: 1, Total: 1})
			emit(progress, ResourceImportEvent{Type: ImportEventDone, Imported: []string{tmpl.Name}})
			return []string{tmpl.Name}, nil
		}
	}

	dir, err := os.MkdirTemp("", "scion-template-reimport-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to open working directory root: %w", err)
	}
	res, fetchErr := client.FetchFolderResolved(ctx, resolvedSrc, resolved.CommitSHA, &tree, cred, root, lim)
	_ = root.Close()
	if fetchErr != nil {
		s.resourceLog.Warn("template reimport: source download failed",
			"owner", src.Owner, "repo", src.Repo, "commit", resolved.CommitSHA, "error", fetchErr)
		return nil, mapTemplateReimportError(fetchErr)
	}

	dirs, _, err := discoverResourceDirs(dir, sourceURL, kind)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion %s found at the source", kind.noun)
	}
	selected, err := selectTemplateSourceDir(dirs, tmpl)
	if err != nil {
		return nil, err
	}
	// Report and write under the target's own name; the store resolves the
	// record by tmpl.ID regardless of the folder name.
	selected.name = tmpl.Name
	selected.sourceCommit = res.CommitSHA
	kind.newStore = func(string) (*ResourceStore, error) { return s.targetTemplateStore(tmpl.ID), nil }
	return s.importResourceDirs(ctx, []resourceDir{selected}, nil, tmpl.Scope, tmpl.ScopeID, kind, progress), nil
}
