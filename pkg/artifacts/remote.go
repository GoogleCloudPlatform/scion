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

package artifacts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
)

// RemoteImageLimits bound the remote images fetched at publish time.
type RemoteImageLimits struct {
	Enabled bool
	// MaxCount caps the images fetched for one version.
	MaxCount int
	// MaxBytes caps one image.
	MaxBytes int64
	// FetchTimeout bounds one fetch; TotalBudget bounds all of them.
	FetchTimeout time.Duration
	TotalBudget  time.Duration
}

// DefaultRemoteImageLimits are used when the host supplies none.
func DefaultRemoteImageLimits() RemoteImageLimits {
	return RemoteImageLimits{
		Enabled: true, MaxCount: 32, MaxBytes: 5 << 20,
		FetchTimeout: 10 * time.Second, TotalBudget: 30 * time.Second,
	}
}

// ImageFetcher fetches one remote image. remotefetch.Fetcher implements it.
type ImageFetcher interface {
	Fetch(ctx context.Context, rawURL string) (*remotefetch.Result, error)
}

// remoteExtractLimit is how many distinct image URLs are kept from one
// document: a few times the fetch cap, so the warning can say roughly how
// many were left out, while the scan stops once it has them.
func remoteExtractLimit(l RemoteImageLimits) int { return 4 * l.MaxCount }

// Publish warnings about the scan for remote images. They sit next to the
// per-image warnings.
const (
	warnBeyondWindow  = "remote images beyond the first 2 MiB of the entry were not fetched"
	warnTooManyImages = "the entry has more images than the hub reads; later remote images were not fetched"
)

// publishDeadlineMargin is the time a publish keeps for storing the fetched
// images, recording the version and writing the response after the fetch
// budget runs out.
const publishDeadlineMargin = 30 * time.Second

// extendWriteDeadline sets the response's write deadline to d from now,
// when the server supports it. It replaces the server-wide write deadline
// for this request, which runs from the end of the request headers and
// would otherwise also have to cover reading the upload and the fetches.
func extendWriteDeadline(w http.ResponseWriter, d time.Duration) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("artifacts: could not extend the publish write deadline", "error", err)
	}
}

// remoteImages finds the remote images of a markdown entry, fetches them
// and returns their manifest rows for version versionID and the publisher's
// warnings. window is the entry's first imageScanWindow bytes and
// truncated tells whether the entry is longer. Nothing is fetched when
// remote images are off or the entry references none.
//
// Remote images count toward the version's file and size limits: used is
// what the version holds without them, and images that would take it past
// either limit are not fetched (or, past the size limit once fetched, not
// kept), with a warning.
func (s *Service) remoteImages(ctx context.Context, w http.ResponseWriter, b backend, versionID, window string, truncated bool, used versionUsage) ([]File, []string) {
	lim := b.remoteImageLimits(ctx)
	if !lim.Enabled {
		return nil, nil
	}
	ex := extractImageURLs(window, remoteExtractLimit(lim))
	var warnings []string
	if truncated {
		warnings = append(warnings, warnBeyondWindow)
	}
	if ex.full && len(ex.urls) <= lim.MaxCount {
		// The scan stopped at its candidate limit, so later images in the
		// entry were not read.
		warnings = append(warnings, warnTooManyImages)
	}
	if len(ex.urls) == 0 {
		return nil, warnings
	}
	vl := b.currentLimits(ctx)
	// Fetching happens inside this request: move its write deadline past
	// the fetch budget so the response is not cut off after the version is
	// recorded.
	extendWriteDeadline(w, lim.TotalBudget+publishDeadlineMargin)
	files, warn := s.fetchRemoteImages(ctx, b, lim, versionID, ex.urls, vl.MaxFiles-used.files, ex.full)
	warnings = append(warnings, warn...)
	left := vl.MaxBundleBytes - used.bytes
	kept := files[:0]
	dropped := 0
	for _, f := range files {
		if f.FetchStatus == FetchStatusOK {
			if f.Size > left {
				dropped++
				continue
			}
			left -= f.Size
		}
		kept = append(kept, f)
	}
	if dropped > 0 {
		warnings = append(warnings, fmt.Sprintf("%d more remote images were not fetched (the version's size limit)", dropped))
	}
	return kept, warnings
}

// versionUsage is what a version holds before its remote images.
type versionUsage struct {
	files int
	bytes int64
}

// remoteFetchConcurrency is how many images of one version are fetched at
// once.
const remoteFetchConcurrency = 4

// remoteFetchFailed is the only failure text stored or shown for a remote
// image. The specific reason is logged on the server and nowhere else, so
// a publisher cannot tell a refused address from a missing image.
const remoteFetchFailed = "image could not be fetched"

func defaultFetcherFactory(l RemoteImageLimits) ImageFetcher {
	return remotefetch.New(remotefetch.Config{MaxBytes: l.MaxBytes, Timeout: l.FetchTimeout})
}

// setImageFetcherFactory replaces the function that builds the fetcher for
// a publish from the current limits. It is unexported so only this
// package's tests can replace the guarded fetcher.
func (s *Service) setImageFetcherFactory(fn func(RemoteImageLimits) ImageFetcher) {
	s.mu.Lock()
	s.fetcherFactory = fn
	s.mu.Unlock()
}

// remoteImageLimits returns the host's remote image limits. Without a
// limits getter the defaults apply; limits the host does supply but that
// are incomplete or invalid turn remote images off (fail closed).
func (b backend) remoteImageLimits(ctx context.Context) RemoteImageLimits {
	if b.limits == nil {
		return DefaultRemoteImageLimits()
	}
	l := b.limits(ctx).RemoteImages
	if l.MaxCount <= 0 || l.MaxBytes <= 0 || l.FetchTimeout <= 0 || l.TotalBudget <= 0 || l.TotalBudget > MaxRemoteFetchBudget {
		return RemoteImageLimits{}
	}
	return l
}

// MaxRemoteFetchBudget caps the total fetch budget the service accepts
// from its host; a larger one turns remote images off. With
// publishDeadlineMargin it bounds how long a publish holds its request
// open for fetching.
const MaxRemoteFetchBudget = 60 * time.Second

// fetchRemoteImages fetches the images at urls into the blob store and
// returns one manifest row per URL it processed, in order, plus the
// warnings for the publisher. A failure never fails the publish: the row is
// marked failed with a generic error and the warning is generic too.
func (s *Service) fetchRemoteImages(ctx context.Context, b backend, lim RemoteImageLimits, versionID string, urls []string, room int, scanFull bool) ([]File, []string) {
	if !lim.Enabled || len(urls) == 0 {
		return nil, nil
	}
	var warnings []string
	if len(urls) > lim.MaxCount {
		more := fmt.Sprintf("%d", len(urls)-lim.MaxCount)
		if scanFull || len(urls) >= remoteExtractLimit(lim) {
			more = "at least " + more
		}
		warnings = append(warnings, fmt.Sprintf("%s more remote images were not fetched (at most %d per version)", more, lim.MaxCount))
		urls = urls[:lim.MaxCount]
	}
	if len(urls) > room {
		warnings = append(warnings, fmt.Sprintf("%d more remote images were not fetched (the version's file limit)", len(urls)-max(room, 0)))
		urls = urls[:max(room, 0)]
	}
	if len(urls) == 0 {
		return nil, warnings
	}
	s.mu.RLock()
	factory := s.fetcherFactory
	s.mu.RUnlock()
	if factory == nil {
		factory = defaultFetcherFactory
	}
	fetcher := factory(lim)

	budget, cancel := context.WithTimeout(ctx, lim.TotalBudget)
	defer cancel()
	phaseStart := time.Now()
	files := make([]File, len(urls))
	floored := make([]bool, len(urls))
	sem := make(chan struct{}, remoteFetchConcurrency)
	var wg sync.WaitGroup
	for i, u := range urls {
		// Take a slot before starting the goroutine, so at most
		// remoteFetchConcurrency goroutines exist at once.
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			defer func() { <-sem }()
			files[i], floored[i] = s.fetchOne(ctx, budget, b, fetcher, versionID, u)
		}(i, u)
	}
	wg.Wait()
	for _, f := range floored {
		if f {
			s.waitFetchFloor(budget, phaseStart)
			break
		}
	}
	for _, f := range files {
		if f.FetchStatus != FetchStatusOK {
			warnings = append(warnings, remoteFetchFailed+": "+f.SourceURL)
		}
	}
	return files, warnings
}

// fetchOne fetches one image under the budget and stores it. Uploads use
// ctx, not the budget, so a fetched image is not lost to the deadline.
//
// The second result reports a fetch that was refused by the address rules
// or whose host could not be resolved; such fetches complete no sooner
// than the fetch floor (see waitFetchFloor).
func (s *Service) fetchOne(ctx, budget context.Context, b backend, fetcher ImageFetcher, versionID, u string) (File, bool) {
	f := File{VersionID: versionID, Path: RemotePath(u), Origin: FileOriginRemote, SourceURL: u}
	res, err := fetcher.Fetch(budget, u)
	if err != nil {
		// The fetcher logs the specific reason.
		f.FetchStatus, f.FetchError = FetchStatusFailed, remoteFetchFailed
		var fe *remotefetch.Error
		floored := errors.As(err, &fe) && (fe.Reason == remotefetch.ReasonDeniedAddress || fe.Reason == remotefetch.ReasonResolve)
		return f, floored
	}
	if err := putBlobBytes(ctx, b, res.SHA256, res.Body, res.ContentType); err != nil {
		slog.ErrorContext(ctx, "artifacts: remote image blob write failed", "error", err)
		f.FetchStatus, f.FetchError = FetchStatusFailed, remoteFetchFailed
		return f, false
	}
	f.Size, f.SHA256, f.MediaType, f.FetchStatus = int64(len(res.Body)), res.SHA256, res.ContentType, FetchStatusOK
	return f, false
}

// waitFetchFloor makes the fetch phase that started at start last at least
// the fetch floor: the fetcher's connect timeout. It runs once per publish,
// however many fetches were refused or unresolvable, and never past the
// fetch budget (budget is the budget's context).
func (s *Service) waitFetchFloor(budget context.Context, start time.Time) {
	s.mu.RLock()
	floor := s.fetchFloor
	s.mu.RUnlock()
	if floor <= 0 {
		floor = remotefetch.DefaultConnectTimeout
	}
	d := floor - time.Since(start)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-budget.Done():
	}
}

// putBlobBytes stores body at its content address (see storeBlob).
func putBlobBytes(ctx context.Context, b backend, digest string, body []byte, mediaType string) error {
	return storeBlob(ctx, b, digest, mediaType, func() (io.Reader, error) { return bytes.NewReader(body), nil })
}
