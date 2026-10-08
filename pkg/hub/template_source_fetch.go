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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
)

// Template reimport downloads a template source with its own fetcher rather
// than the general import path. The source must be a folder in a public
// GitHub repository; the hub builds the archive URL itself, and every request
// (including GitHub's redirect to its download host) is limited to the hosts
// below, vetted for public addresses, bounded in redirects, size and time.
// The archive is then extracted with its own size and file-count limits.

// templateSourceHosts is the complete set of hosts a template reimport may
// contact. github.com serves the archive URL and redirects to codeload.
var templateSourceHosts = []string{"github.com", "codeload.github.com"}

// templateSourceLimits bounds one template source download and extraction.
type templateSourceLimits struct {
	// MaxDownload caps the compressed archive size.
	MaxDownload int64
	// MaxUnpacked caps the total uncompressed archive stream read, including
	// entries outside the requested folder.
	MaxUnpacked int64
	// MaxExtract caps the total size of files written for the requested folder.
	MaxExtract int64
	// MaxFiles caps the number of files written.
	MaxFiles int
	// Timeout bounds the download.
	Timeout time.Duration
	// MaxRedirects bounds redirect hops.
	MaxRedirects int
}

var defaultTemplateSourceLimits = templateSourceLimits{
	MaxDownload:  64 << 20,
	MaxUnpacked:  512 << 20,
	MaxExtract:   32 << 20,
	MaxFiles:     2000,
	Timeout:      2 * time.Minute,
	MaxRedirects: 3,
}

// templateSourceFetcher downloads one body. *remotefetch.Fetcher satisfies it.
type templateSourceFetcher interface {
	FetchBytes(ctx context.Context, rawURL string) (*remotefetch.Result, error)
}

// newTemplateSourceFetcher returns the fetcher template reimport uses: https
// only, limited to templateSourceHosts on every hop, public addresses only,
// with the download size, timeout and redirect limits applied.
func newTemplateSourceFetcher(lim templateSourceLimits, logger *slog.Logger) *remotefetch.Fetcher {
	return remotefetch.New(remotefetch.Config{
		MaxBytes:     lim.MaxDownload,
		Timeout:      lim.Timeout,
		MaxRedirects: lim.MaxRedirects,
		AllowedHosts: templateSourceHosts,
		Logger:       logger,
	})
}

func (s *Server) getTemplateSourceFetcher() templateSourceFetcher {
	if s.templateSourceFetcher != nil {
		return s.templateSourceFetcher
	}
	return newTemplateSourceFetcher(defaultTemplateSourceLimits, s.resourceLog)
}

// templateGitHubSource is a validated GitHub folder URL.
type templateGitHubSource struct {
	Owner  string
	Repo   string
	Branch string
	// Path is the folder within the repository, "" for the repository root.
	Path string
}

var (
	githubOwnerRe  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	githubRepoRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	githubBranchRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
	githubPathRe   = regexp.MustCompile(`^[A-Za-z0-9._ @+-]{1,255}$`)
)

// errTemplateSourceUnsupported is returned for a source URL reimport does not
// support. Its message is fixed text and never includes the URL.
var errTemplateSourceUnsupported = errors.New(
	"refreshing from source is only supported for templates imported from a GitHub folder URL (https://github.com/<owner>/<repo>/tree/<branch>/<path>)")

// parseTemplateGitHubSource validates raw as an https://github.com folder URL
// with no username, password, port other than 443, query or fragment, and
// returns its parts.
func parseTemplateGitHubSource(raw string) (*templateGitHubSource, error) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Opaque != "" {
		return nil, errTemplateSourceUnsupported
	}
	if u.User != nil {
		return nil, errors.New("source URL must not include a username or password")
	}
	if !strings.EqualFold(u.Hostname(), "github.com") || (u.Port() != "" && u.Port() != "443") {
		return nil, errTemplateSourceUnsupported
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errTemplateSourceUnsupported
	}

	var parts []string
	for _, p := range strings.Split(u.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return nil, errTemplateSourceUnsupported
	}
	src := &templateGitHubSource{Owner: parts[0], Repo: strings.TrimSuffix(parts[1], ".git"), Branch: "main"}
	rest := parts[2:]
	if len(rest) > 0 && rest[0] == "tree" {
		if len(rest) < 2 {
			return nil, errTemplateSourceUnsupported
		}
		src.Branch = rest[1]
		rest = rest[2:]
	} else if len(rest) > 0 && (rest[0] == "archive" || rest[0] == "blob" || rest[0] == "releases") {
		return nil, errTemplateSourceUnsupported
	}

	if !githubOwnerRe.MatchString(src.Owner) || !githubRepoRe.MatchString(src.Repo) ||
		src.Repo == "." || src.Repo == ".." || !githubBranchRe.MatchString(src.Branch) ||
		strings.Contains(src.Branch, "..") {
		return nil, errTemplateSourceUnsupported
	}
	for _, p := range rest {
		if p == "." || p == ".." || !githubPathRe.MatchString(p) {
			return nil, errTemplateSourceUnsupported
		}
	}
	src.Path = strings.Join(rest, "/")
	return src, nil
}

// archiveURL is the GitHub archive URL for the source's branch.
func (g *templateGitHubSource) archiveURL() string {
	return "https://github.com/" + g.Owner + "/" + g.Repo + "/archive/refs/heads/" + url.PathEscape(g.Branch) + ".tar.gz"
}

// errTemplateSourceTooLarge reports that a source exceeded an extraction limit.
var errTemplateSourceTooLarge = errors.New("template source exceeds the size limit")

// cappedReader returns errTemplateSourceTooLarge once more than remaining
// bytes have been read.
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

// extractTemplateSource extracts the files under subPath from a GitHub
// archive (a gzip tarball with one top-level directory) into dest. Only
// regular files and directories are written; links and other entry types
// are skipped, as are entries whose names would leave dest. It fails when
// the archive exceeds the unpacked, extracted-size or file-count limits, or
// when nothing exists at subPath.
func extractTemplateSource(archive []byte, subPath, dest string, lim templateSourceLimits) error {
	gzr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("template source is not a valid archive: %w", err)
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(&cappedReader{r: gzr, remaining: lim.MaxUnpacked})

	subPath = strings.Trim(subPath, "/")
	var written int64
	files := 0
	found := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, errTemplateSourceTooLarge) {
				return errTemplateSourceTooLarge
			}
			return fmt.Errorf("template source is not a valid archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}

		// Drop the archive's single top-level directory.
		name := path.Clean("/" + hdr.Name)
		_, rel, ok := strings.Cut(strings.TrimPrefix(name, "/"), "/")
		if !ok || rel == "" {
			continue
		}
		if subPath != "" {
			if rel == subPath {
				rel = ""
			} else if strings.HasPrefix(rel, subPath+"/") {
				rel = strings.TrimPrefix(rel, subPath+"/")
			} else {
				continue
			}
		}
		found = true
		if rel == "" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if r, err := filepath.Rel(dest, target); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			continue
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		files++
		if files > lim.MaxFiles {
			return errTemplateSourceTooLarge
		}
		if hdr.Size < 0 || written+hdr.Size > lim.MaxExtract {
			return errTemplateSourceTooLarge
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(f, io.LimitReader(tr, lim.MaxExtract-written+1))
		closeErr := f.Close()
		written += n
		if copyErr != nil {
			if errors.Is(copyErr, errTemplateSourceTooLarge) {
				return errTemplateSourceTooLarge
			}
			return fmt.Errorf("template source is not a valid archive: %w", copyErr)
		}
		if written > lim.MaxExtract {
			return errTemplateSourceTooLarge
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if !found {
		return errors.New("the source folder was not found in the repository")
	}
	return nil
}

// templateSourceFetchError turns a download failure into a message that is
// safe to return to the caller. Details stay in the server log.
func templateSourceFetchError(err error) error {
	var fe *remotefetch.Error
	if errors.As(err, &fe) {
		switch fe.Reason {
		case remotefetch.ReasonStatus:
			return errors.New("GitHub did not return the source archive; the repository or branch may not exist or may not be public")
		case remotefetch.ReasonTooLarge:
			return errTemplateSourceTooLarge
		case remotefetch.ReasonTimeout:
			return errors.New("timed out downloading the template source")
		}
	}
	return errors.New("could not download the template source")
}

// reimportTemplateSource downloads src, extracts it and create-or-syncs the
// templates named in names into the given scope. sourceURL is the validated
// source URL recorded on the refreshed template.
func (s *Server) reimportTemplateSource(ctx context.Context, src *templateGitHubSource, sourceURL, scope, scopeID string, names []string, progress importProgressFunc) ([]string, error) {
	return s.reimportTemplateSourceWithLimits(ctx, src, sourceURL, scope, scopeID, names, progress, defaultTemplateSourceLimits)
}

func (s *Server) reimportTemplateSourceWithLimits(ctx context.Context, src *templateGitHubSource, sourceURL, scope, scopeID string, names []string, progress importProgressFunc, lim templateSourceLimits) ([]string, error) {
	kind := s.templateImportKind()
	if s.GetStorage() == nil {
		return nil, fmt.Errorf("%s storage is not configured", kind.noun)
	}

	res, err := s.getTemplateSourceFetcher().FetchBytes(ctx, src.archiveURL())
	if err != nil {
		s.resourceLog.Warn("template reimport: source download failed",
			"owner", src.Owner, "repo", src.Repo, "error", err)
		return nil, templateSourceFetchError(err)
	}
	if int64(len(res.Body)) > lim.MaxDownload {
		return nil, errTemplateSourceTooLarge
	}

	dir, err := os.MkdirTemp("", "scion-template-reimport-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if err := extractTemplateSource(res.Body, src.Path, dir, lim); err != nil {
		return nil, err
	}

	dirs, skipped, err := discoverResourceDirs(dir, sourceURL, kind)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no scion %s found at the source", kind.noun)
	}
	dirs, skipped = applyNameFilter(dirs, skipped, names)
	if len(dirs) == 0 {
		return nil, fmt.Errorf("the source does not contain this template")
	}
	return s.importResourceDirs(ctx, dirs, skipped, scope, scopeID, kind, progress), nil
}
