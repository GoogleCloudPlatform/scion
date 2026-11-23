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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Limits of one hosted server-side template or harness-config source fetch
// (import, discover, harness-config reimport and skill discovery; see
// fetchRemoteForImport). Template "Refresh from Source" has its own fetcher
// and limits (defaultTemplateSourceLimits).
const (
	// importFetchTimeout bounds the whole fetch: download, extraction, and
	// Cloud Storage copies.
	importFetchTimeout = 5 * time.Minute
	// importDownloadTimeout bounds one HTTP download.
	importDownloadTimeout = 2 * time.Minute
	// importMaxDownloadBytes caps one HTTP download (the compressed archive
	// or GitHub tarball).
	importMaxDownloadBytes int64 = 64 << 20
	// importMaxRedirects bounds redirect hops on GitHub tarball downloads
	// (github.com/api.github.com -> codeload.github.com only). Non-GitHub
	// allowlisted HTTPS downloads block all redirects.
	importMaxRedirects = 3
	// importMaxUnpackedBytes caps the decompressed bytes read from an
	// archive in each pass over it.
	importMaxUnpackedBytes int64 = 512 << 20
	// importMaxTreeBytes caps the file bytes written for one fetch,
	// including a GitHub tarball's temporary extraction of the whole
	// repository.
	importMaxTreeBytes int64 = 256 << 20
	// importMaxTreeFiles caps the files and directories written for one
	// fetch, counted the same way.
	importMaxTreeFiles = 20000
)

// importDownloadOverride, when set, replaces the SSRF-safe downloader for
// hosted server-side import fetches. It exists for the pkg/hub tests, whose
// sources are served by test servers on loopback; production code never
// sets it.
var importDownloadOverride config.RemoteDownloadFunc

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

// fetchRemoteForImport fetches a remote source URL to a local cache directory,
// authenticating against GitHub with the credential importCredential selects
// for the import's scope ("global", "project" or "user") and scopeID. It
// applies to both resource kinds (templates and harness-configs) and to skill
// directory discovery. The caller owns the returned cache path and must
// remove it.
//
// On a workstation Hub, fetching uses the default HTTP client without hosted
// source restrictions. On a hosted Hub, the source must pass checkImportSource
// (https://github.com, or a source server.hub.allowed_import_sources allows),
// HTTP downloads go through remotefetch (https on port 443 to public addresses
// only; redirects are blocked on non-GitHub allowlisted HTTPS sources and
// pinned to templateSourceHosts on GitHub fetches), and the fetch is bounded
// by the import* limits above with no sparse-checkout fallback.
func (s *Server) fetchRemoteForImport(ctx context.Context, scope, scopeID, sourceURL string) (string, error) {
	res, err := s.fetchRemoteForImportWithResult(ctx, scope, scopeID, sourceURL)
	if err != nil {
		return "", err
	}
	return res.Path, nil
}

func (s *Server) fetchRemoteForImportWithResult(ctx context.Context, scope, scopeID, sourceURL string) (*config.RemoteFetchResult, error) {
	if s.workstation {
		return config.FetchRemoteTemplateWithResult(ctx, sourceURL, config.FetchOptions{
			AuthToken:       s.importCredential(ctx, scope, scopeID, sourceURL),
			PrivateCacheDir: true,
		})
	}
	fetchURI, err := s.checkImportSource(sourceURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, importFetchTimeout)
	defer cancel()
	res, err := config.FetchRemoteTemplateWithResult(ctx, fetchURI, config.FetchOptions{
		AuthToken:        s.importCredential(ctx, scope, scopeID, fetchURI),
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

// errImportGCSFetchFailed is the caller-facing error for a failed Cloud
// Storage fetch.
var errImportGCSFetchFailed = errors.New("the Cloud Storage source could not be read; check the path and that the Hub has access to it")

// importDownloader returns the download hook for hosted server-side import
// fetches.
func (s *Server) importDownloader() config.RemoteDownloadFunc {
	if importDownloadOverride != nil {
		return importDownloadOverride
	}
	return newImportDownloader(s.resourceLog)
}

// newImportDownloader returns the SSRF-safe download hook for hosted Hubs.
// GitHub downloads (github.com, with or without a credential) are restricted
// to templateSourceHosts (github.com, api.github.com, codeload.github.com) on
// every hop so GitHub's tarball redirect to codeload works while blocking any
// redirect elsewhere. Allowlisted non-GitHub HTTPS downloads block all
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
	github := remotefetch.New(remotefetch.Config{
		MaxBytes:     importMaxDownloadBytes,
		Timeout:      importDownloadTimeout,
		MaxRedirects: importMaxRedirects,
		AllowedHosts: templateSourceHosts,
		Logger:       logger,
	})
	return func(ctx context.Context, rawURL, authorization string) ([]byte, error) {
		var res *remotefetch.Result
		var err error
		if authorization != "" {
			res, err = github.FetchBytesAuthorized(ctx, rawURL, authorization)
		} else if isGitHubDownloadURL(rawURL) {
			res, err = github.FetchBytes(ctx, rawURL)
		} else {
			res, err = direct.FetchBytes(ctx, rawURL)
		}
		if err != nil {
			return nil, importDownloadError(err)
		}
		return res.Body, nil
	}
}

// isGitHubDownloadURL reports whether rawURL targets github.com over https.
func isGitHubDownloadURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "github.com")
}

// errImportDownloadFailed is the caller-facing error for a failed import
// download. remotefetch logs the reason and detail server-side; the caller
// never learns whether an address was refused, a status code, or a body.
var errImportDownloadFailed = errors.New("the source could not be downloaded; check the URL and that the source is public or a repository credential is set")

// importDownloadError maps a remotefetch error to a fixed message.
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
