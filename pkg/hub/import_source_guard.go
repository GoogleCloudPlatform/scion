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
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
)

// Limits of one hosted server-side non-GitHub import source fetch (allowlisted
// archive URLs and Cloud Storage). GitHub folder imports use
// githubfetch.ImportHostedLimits() on hosted Hubs and
// githubfetch.ImportLocalLimits() on workstation Hubs.
const (
	// importFetchTimeout bounds the whole non-GitHub fetch: download,
	// extraction, and Cloud Storage copies.
	importFetchTimeout = 5 * time.Minute
	// importDownloadTimeout bounds one non-GitHub HTTP download.
	importDownloadTimeout = 2 * time.Minute
	// importMaxDownloadBytes caps one non-GitHub HTTP archive download.
	importMaxDownloadBytes int64 = 64 << 20
	// importMaxUnpackedBytes caps the decompressed bytes read from a
	// non-GitHub archive in each pass over it.
	importMaxUnpackedBytes int64 = 512 << 20
	// importMaxTreeBytes caps the file bytes written for one non-GitHub fetch.
	importMaxTreeBytes int64 = 256 << 20
	// importMaxTreeFiles caps the files and directories written for one
	// non-GitHub fetch.
	importMaxTreeFiles = 20000
	// importGitHubArchiveMaxRedirects bounds redirects when following
	// https://github.com/... archive URLs to codeload.github.com.
	importGitHubArchiveMaxRedirects = 3
)

// githubArchiveHosts is the host allowlist for hosted Hub downloads of
// user-supplied https://github.com/... archive URLs (which redirect to
// codeload.github.com).
var githubArchiveHosts = []string{"github.com", "codeload.github.com"}

// importDownloadOverride, when set, replaces the SSRF-safe downloader for
// hosted server-side non-GitHub archive fetches. It exists for the pkg/hub
// tests, whose sources are served by test servers on loopback; production code
// never sets it.
var importDownloadOverride config.RemoteDownloadFunc

// importGitHubClientOverride, when set, replaces the default githubfetch.Client
// for Hub GitHub import/discover/reimport fetches when s.templateGitHubClient
// is nil. It exists for pkg/hub tests; production code never sets it.
var importGitHubClientOverride *githubfetch.Client

// checkImportSource returns the URI to fetch for sourceURL, or an error
// (wrapping config.ErrImportSourceNotAllowed, without the URL) when a hosted
// Hub may not fetch it.
//
// A workstation Hub is exempt and accepts every remote URI as before.
func (s *Server) checkImportSource(sourceURL string) (string, error) {
	if s.workstation {
		return sourceURL, nil
	}
	return config.CheckHubImportSource(sourceURL, s.config.AllowedImportSources)
}

// refuseImportSource writes a 400 invalid_request answer and returns true when
// checkImportSource refuses sourceURL. Handlers call it before fetching, so the
// refusal also reaches streaming (NDJSON) callers as a plain error answer.
func (s *Server) refuseImportSource(w http.ResponseWriter, sourceURL string) bool {
	_, err := s.checkImportSource(sourceURL)
	if err == nil {
		return false
	}
	msg := config.ErrImportSourceNotAllowed.Error()
	if errors.Is(err, config.ErrImportSourceNotAllowed) {
		msg = err.Error()
	}
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, msg, nil)
	return true
}

// importFetchError carries a classified githubfetch failure with a caller-safe
// fixed message and optional Retry-After duration for 429 responses.
type importFetchError struct {
	Kind       githubfetch.Kind
	RetryAfter time.Duration
	Message    string
	wrapped    error
}

func (e *importFetchError) Error() string { return e.Message }
func (e *importFetchError) Unwrap() error { return e.wrapped }

// mapGitHubImportError maps a githubfetch error into a caller-safe fixed
// message. Raw error details, tokens, and redirect URLs stay in server logs.
func mapGitHubImportError(err error) error {
	var ghe *githubfetch.Error
	if !errors.As(err, &ghe) {
		return &importFetchError{
			Kind:    githubfetch.KindUnreachable,
			Message: "could not download the GitHub source",
		}
	}
	switch ghe.Kind {
	case githubfetch.KindNotFound:
		msg := "GitHub repository, ref, or folder was not found, or is private and no repository credential is configured"
		if ghe.CredKind != githubfetch.CredAnonymous {
			msg = "GitHub repository, ref, or folder was not found, or the configured repository credential cannot read it"
		}
		if ghe.Hint != "" {
			msg += " (" + ghe.Hint + ")"
		}
		return &importFetchError{Kind: ghe.Kind, Message: msg}
	case githubfetch.KindAuthRejected:
		msg := "GitHub rejected the configured repository credential (it may be invalid or expired)"
		if ghe.Hint != "" {
			msg += " (" + ghe.Hint + ")"
		}
		return &importFetchError{Kind: ghe.Kind, Message: msg}
	case githubfetch.KindForbidden:
		msg := "GitHub denied access to the repository"
		if ghe.Hint != "" {
			msg += " (" + ghe.Hint + ")"
		}
		return &importFetchError{Kind: ghe.Kind, Message: msg}
	case githubfetch.KindRateLimited:
		return &importFetchError{
			Kind:       ghe.Kind,
			RetryAfter: ghe.RetryAfter,
			Message:    "GitHub rate limit exceeded; retry later",
		}
	case githubfetch.KindTooLarge:
		return &importFetchError{
			Kind:    ghe.Kind,
			Message: "GitHub source exceeds the size or file count limit",
			wrapped: config.ErrRemoteFetchLimit,
		}
	case githubfetch.KindTimeout:
		return &importFetchError{
			Kind:    ghe.Kind,
			Message: "timed out downloading the GitHub source",
			wrapped: config.ErrRemoteDownloadTimeout,
		}
	case githubfetch.KindUnsupportedContent:
		return &importFetchError{
			Kind:    ghe.Kind,
			Message: "GitHub source contains unsupported content",
		}
	case githubfetch.KindUpstream:
		return &importFetchError{
			Kind:    ghe.Kind,
			Message: "GitHub returned an invalid or inconsistent archive",
		}
	case githubfetch.KindInvalidSource:
		msg := "invalid GitHub source URL"
		if ghe.Hint == "source URL must not include a username or password" {
			msg = ghe.Hint
		}
		return &importFetchError{Kind: ghe.Kind, Message: msg}
	default:
		return &importFetchError{
			Kind:    ghe.Kind,
			Message: "could not download the GitHub source",
		}
	}
}

// writeImportRateLimitError writes a 429 rate_limited response with Retry-After
// and returns true when err is a rate-limited GitHub fetch error.
func writeImportRateLimitError(w http.ResponseWriter, err error) bool {
	var retryAfter time.Duration
	var msg string
	var ife *importFetchError
	if errors.As(err, &ife) && ife.Kind == githubfetch.KindRateLimited {
		retryAfter = ife.RetryAfter
		msg = ife.Message
	} else {
		var rfe *reimportFetchError
		if errors.As(err, &rfe) && rfe.Kind == githubfetch.KindRateLimited {
			retryAfter = rfe.RetryAfter
			msg = rfe.Message
		} else {
			return false
		}
	}
	secs := int64(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = int64(githubfetch.CooldownDefault / time.Second)
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	writeError(w, http.StatusTooManyRequests, "rate_limited", msg, nil)
	return true
}

// importFetchErrorMessage returns the clean caller-safe fixed message when err
// wraps a classified importFetchError, or err.Error() otherwise.
func importFetchErrorMessage(err error) string {
	var ife *importFetchError
	if errors.As(err, &ife) {
		return ife.Message
	}
	return err.Error()
}

// fetchRemoteForImport fetches a remote source URL to a private per-call
// staging directory, authenticating against GitHub with the credential
// githubCredential selects for the import's scope ("global", "project" or
// "user") and scopeID. It applies to both resource kinds (templates and
// harness-configs) and to skill directory discovery. The caller owns the
// returned cache path and must remove it.
//
// GitHub folder sources always go through pkg/githubfetch: hosted Hubs use
// remotefetch.NetworkVetted + githubfetch.ImportHostedLimits(); workstation
// Hubs use remotefetch.NetworkSystem + githubfetch.ImportLocalLimits().
// Non-GitHub sources (archive URLs, rclone, allowed_import_sources HTTPS) keep
// their existing pkg/config fetchers.
func (s *Server) fetchRemoteForImport(ctx context.Context, scope, scopeID, sourceURL string) (string, error) {
	res, err := s.fetchRemoteForImportWithResult(ctx, scope, scopeID, sourceURL)
	if err != nil {
		return "", err
	}
	return res.Path, nil
}

func (s *Server) fetchRemoteForImportWithResult(ctx context.Context, scope, scopeID, sourceURL string) (*config.RemoteFetchResult, error) {
	fetchURI := sourceURL
	if !s.workstation {
		var err error
		fetchURI, err = s.checkImportSource(sourceURL)
		if err != nil {
			return nil, err
		}
	}

	if config.DetectRemoteType(fetchURI) == config.RemoteTypeGitHub {
		return s.fetchGitHubForImport(ctx, scope, scopeID, fetchURI)
	}

	if s.workstation {
		return config.FetchRemoteTemplateWithResult(ctx, fetchURI, config.FetchOptions{
			PrivateCacheDir: true,
		})
	}

	ctx, cancel := context.WithTimeout(ctx, importFetchTimeout)
	defer cancel()
	res, err := config.FetchRemoteTemplateWithResult(ctx, fetchURI, config.FetchOptions{
		Download:         s.importDownloader(),
		MaxBytes:         importMaxTreeBytes,
		MaxFiles:         importMaxTreeFiles,
		MaxUnpackedBytes: importMaxUnpackedBytes,
		PrivateCacheDir:  true,
	})
	if err != nil && strings.HasPrefix(fetchURI, ":gcs:") && !errors.Is(err, config.ErrRemoteFetchLimit) {
		// Cloud Storage errors can name the Hub's service account and
		// project; keep them in the Hub log.
		if s.resourceLog != nil {
			s.resourceLog.Warn("import fetch from cloud storage failed", "source", fetchURI, "error", err)
		}
		return nil, errImportGCSFetchFailed
	}
	return res, err
}

func (s *Server) fetchGitHubForImport(ctx context.Context, scope, scopeID, fetchURI string) (*config.RemoteFetchResult, error) {
	src, err := githubfetch.ParseURL(fetchURI)
	if err != nil {
		return nil, mapGitHubImportError(err)
	}

	lim := s.templateReimportLimits()
	if lim.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lim.Timeout)
		defer cancel()
	}

	cred := s.githubCredential(ctx, ghCredRequest{
		Scope:   scope,
		ScopeID: scopeID,
		Owner:   src.Owner,
		Repo:    src.Repo,
	})

	client := s.getImportGitHubClient(lim)
	res, err := config.FetchRemoteTemplateWithResult(ctx, fetchURI, config.FetchOptions{
		GitHubCredential: cred,
		GitHubClient:     client,
		GitHubLimits:     &lim,
		PrivateCacheDir:  true,
	})
	if err != nil {
		kind := ""
		var ghe *githubfetch.Error
		if errors.As(err, &ghe) {
			kind = string(ghe.Kind)
		}
		if s.resourceLog != nil {
			s.resourceLog.Warn("GitHub import fetch failed",
				"owner", src.Owner, "repo", src.Repo, "kind", kind, "error", err)
		}
		return nil, mapGitHubImportError(err)
	}
	return res, nil
}

func (s *Server) getImportGitHubClient(lim githubfetch.Limits) *githubfetch.Client {
	if s.templateGitHubClient == nil && importGitHubClientOverride != nil {
		return importGitHubClientOverride
	}
	return s.getTemplateGitHubClient(lim)
}

// errImportGCSFetchFailed is the caller-facing error for a failed Cloud
// Storage fetch.
var errImportGCSFetchFailed = errors.New("the Cloud Storage source could not be read; check the path and that the Hub has access to it")

// importDownloader returns the download hook for hosted server-side non-GitHub
// archive fetches.
func (s *Server) importDownloader() config.RemoteDownloadFunc {
	if importDownloadOverride != nil {
		return importDownloadOverride
	}
	return newImportDownloader(s.resourceLog)
}

type importBytesFetcher interface {
	FetchBytes(ctx context.Context, rawURL string) (*remotefetch.Result, error)
}

// githubArchiveFetchConfig returns the remotefetch.Config used by hosted Hubs
// for user-supplied https://github.com/... archive downloads (redirects are
// restricted to github.com and codeload.github.com, up to 3 hops, vetted
// network, no Authorization).
func githubArchiveFetchConfig(logger *slog.Logger) remotefetch.Config {
	if logger == nil {
		logger = slog.Default()
	}
	return remotefetch.Config{
		MaxBytes:     importMaxDownloadBytes,
		Timeout:      importDownloadTimeout,
		MaxRedirects: importGitHubArchiveMaxRedirects,
		AllowedHosts: githubArchiveHosts,
		Network:      remotefetch.NetworkVetted,
		Logger:       logger,
	}
}

// newImportDownloader returns the SSRF-safe download hook for hosted Hubs
// fetching archive sources. User-supplied https://github.com/... archive
// downloads follow redirects only within githubArchiveHosts (github.com and
// codeload.github.com, up to 3 hops, NetworkVetted, never sending
// Authorization). Allowlisted non-GitHub HTTPS archive downloads block all
// redirects.
func newImportDownloader(logger *slog.Logger) config.RemoteDownloadFunc {
	if logger == nil {
		logger = slog.Default()
	}
	direct := remotefetch.New(remotefetch.Config{
		MaxBytes:         importMaxDownloadBytes,
		Timeout:          importDownloadTimeout,
		DisableRedirects: true,
		Logger:           logger,
	})
	githubArchive := remotefetch.New(githubArchiveFetchConfig(logger))
	return newImportDownloaderWithFetchers(direct, githubArchive)
}

func newImportDownloaderWithFetchers(direct, githubArchive importBytesFetcher) config.RemoteDownloadFunc {
	return func(ctx context.Context, rawURL, _ string) ([]byte, error) {
		var (
			res *remotefetch.Result
			err error
		)
		if isGitHubArchiveDownloadURL(rawURL) {
			res, err = githubArchive.FetchBytes(ctx, rawURL)
		} else {
			res, err = direct.FetchBytes(ctx, rawURL)
		}
		if err != nil {
			return nil, importDownloadError(err)
		}
		return res.Body, nil
	}
}

// isGitHubArchiveDownloadURL reports whether rawURL targets github.com over
// https (for example https://github.com/<owner>/<repo>/archive/<ref>.tar.gz).
func isGitHubArchiveDownloadURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "github.com")
}

// errImportDownloadFailed is the caller-facing error for a failed non-GitHub
// archive download. remotefetch logs the reason and detail server-side; the
// caller never learns whether an address was refused, a status code, or a body.
var errImportDownloadFailed = errors.New("the source could not be downloaded; check the URL and that the source is public or a repository credential is set")

// importDownloadError maps a remotefetch error on a non-GitHub archive
// download to a fixed message.
func importDownloadError(err error) error {
	var fe *remotefetch.Error
	if errors.As(err, &fe) {
		switch fe.Reason {
		case remotefetch.ReasonTooLarge:
			return fmt.Errorf("%w (the download is larger than %d MiB)", config.ErrRemoteFetchLimit, importMaxDownloadBytes>>20)
		case remotefetch.ReasonTimeout:
			return config.ErrRemoteDownloadTimeout
		}
	}
	return errImportDownloadFailed
}
