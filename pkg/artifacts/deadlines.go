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
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Per-phase upload deadline scaling for handlers that read a potentially large
// request body and store blobs before answering (ptone/scion#9648).
const (
	uploadDeadlineFloor       = 60 * time.Second
	uploadDeadlineMax         = 30 * time.Minute
	uploadDeadlineBytesPerSec = 1 << 20 // 1 MiB/s
)

// uploadDeadlineNow is the clock used by ExtendUploadDeadlines. A variable so
// package tests can assert exact deadlines.
var uploadDeadlineNow = time.Now

// UploadDeadline returns the per-phase deadline budget for transferring size
// bytes: uploadDeadlineFloor plus size at uploadDeadlineBytesPerSec, capped at
// uploadDeadlineMax. Non-positive sizes return uploadDeadlineFloor.
func UploadDeadline(size int64) time.Duration {
	if size <= 0 {
		return uploadDeadlineFloor
	}
	maxExtra := uploadDeadlineMax - uploadDeadlineFloor
	maxBytes := int64(maxExtra/time.Second) * uploadDeadlineBytesPerSec
	if size >= maxBytes {
		return uploadDeadlineMax
	}
	extra := time.Duration(size/uploadDeadlineBytesPerSec) * time.Second
	return uploadDeadlineFloor + extra
}

// ExpectedUploadBytes returns the expected payload size for deadline scaling:
// contentLength when known (clamped to maxBytes when maxBytes > 0), or
// maxBytes when contentLength is unknown (< 0, e.g. chunked transfer).
func ExpectedUploadBytes(contentLength, maxBytes int64) int64 {
	if contentLength < 0 {
		return maxBytes
	}
	if maxBytes > 0 && contentLength > maxBytes {
		return maxBytes
	}
	return contentLength
}

// ExtendUploadDeadlines moves the connection's read and write deadlines ahead
// so a large upload and its subsequent blob write and response are not cut off
// by the serving http.Server's ReadTimeout or WriteTimeout (ptone/scion#9648).
//
// When served by an *http.Server (present in ctx via http.ServerContextKey),
// each deadline is extended only if that server's corresponding timeout is
// positive and shorter than the computed budget, so an unbounded (0) or longer
// server timeout is never shortened. ResponseWriters without deadline support
// (such as httptest.ResponseRecorder) are ignored without logging.
func ExtendUploadDeadlines(ctx context.Context, w http.ResponseWriter, expectedBytes int64) {
	readBudget := UploadDeadline(expectedBytes)
	// WriteTimeout starts once request headers are read and must cover reading
	// the upload body, writing the blob(s) to storage, recording metadata, and
	// writing the HTTP response.
	writeBudget := 2*readBudget + publishDeadlineMargin
	now := uploadDeadlineNow()
	rc := http.NewResponseController(w)

	hs, _ := ctx.Value(http.ServerContextKey).(*http.Server)
	if hs == nil || (hs.ReadTimeout > 0 && hs.ReadTimeout < readBudget) {
		if err := rc.SetReadDeadline(now.Add(readBudget)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			slog.WarnContext(ctx, "artifacts: could not extend the upload read deadline", "error", err)
		}
	}
	if hs == nil || (hs.WriteTimeout > 0 && hs.WriteTimeout < writeBudget) {
		if err := rc.SetWriteDeadline(now.Add(writeBudget)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			slog.WarnContext(ctx, "artifacts: could not extend the upload write deadline", "error", err)
		}
	}
}
