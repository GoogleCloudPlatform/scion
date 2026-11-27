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

package shipper

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	// DefaultMaxSegmentBytes is the default raw byte cap per segment (16 MiB).
	DefaultMaxSegmentBytes int64 = 16 * 1024 * 1024

	// DefaultMaxRoundBytes is the default raw byte cap per RunRound call (64 MiB).
	DefaultMaxRoundBytes int64 = 64 * 1024 * 1024

	// ContentEncodingGzip is the segment compression encoding defined by the
	// wire contract.
	ContentEncodingGzip = "gzip"
)

// Segment holds a prepared, gzip-compressed raw byte range and all integrity
// metadata required by the Hub segments:prepare / PUT / segments:commit flow.
type Segment struct {
	Harness            string
	NativeRelativePath string
	ArtifactKind       ArtifactKind
	Generation         int
	ByteStart          int64
	ByteEnd            int64
	RawSize            int64
	RawSHA256          string
	Compressed         []byte
	CompressedSize     int64
	CompressedSHA256   string
	ContentEncoding    string
	IncompleteTail     bool
	SessionID          string
	OriginHubID        string
}

// PlanSegment computes the next raw byte slice to ship from r starting at
// offset, bounded by maxSegmentBytes and roundBudget.
//
// For JSONL append files:
//   - Non-final rounds ship [offset, last_newline+1), holding back any trailing
//     partial line unless a single line exceeds maxSegmentBytes (in which case
//     it is split at maxSegmentBytes with incompleteTail=true).
//   - Final rounds (final=true) also ship any remaining partial tail at EOF
//     with incompleteTail=true.
//
// ok is false when there are no shippable bytes right now.
func PlanSegment(
	r io.ReaderAt,
	fileSize int64,
	offset int64,
	maxSegmentBytes int64,
	roundBudget int64,
	final bool,
) (raw []byte, byteEnd int64, incompleteTail bool, ok bool, err error) {
	if maxSegmentBytes <= 0 {
		maxSegmentBytes = DefaultMaxSegmentBytes
	}
	if fileSize <= offset || roundBudget <= 0 {
		return nil, offset, false, false, nil
	}

	avail := fileSize - offset
	capBytes := min(maxSegmentBytes, roundBudget)
	readLen := min(avail, capBytes)
	if readLen <= 0 {
		return nil, offset, false, false, nil
	}

	buf := make([]byte, readLen)
	n, readErr := r.ReadAt(buf, offset)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, offset, false, false, readErr
	}
	buf = buf[:n]
	if len(buf) == 0 {
		return nil, offset, false, false, nil
	}

	atEOF := offset+int64(len(buf)) >= fileSize
	lastNL := bytes.LastIndexByte(buf, '\n')

	if lastNL >= 0 {
		if final && atEOF {
			return buf, offset + int64(len(buf)), lastNL != len(buf)-1, true, nil
		}
		cut := lastNL + 1
		return buf[:cut], offset + int64(cut), false, true, nil
	}

	// No newline within buf.
	if int64(len(buf)) == maxSegmentBytes {
		// Single line exceeds maxSegmentBytes: split at the segment cap.
		return buf, offset + int64(len(buf)), true, true, nil
	}
	if final && atEOF {
		return buf, offset + int64(len(buf)), true, true, nil
	}
	// Hold back partial line (or wait for a fresh round budget).
	return nil, offset, false, false, nil
}

// SegmentMeta supplies the non-content metadata for BuildSegment.
type SegmentMeta struct {
	Harness            string
	NativeRelativePath string
	ArtifactKind       ArtifactKind
	Generation         int
	ByteStart          int64
	IncompleteTail     bool
	SessionID          string
	OriginHubID        string
}

// BuildSegment compresses raw bytes with deterministic gzip framing and
// computes raw and compressed SHA-256 digests.
func BuildSegment(meta SegmentMeta, raw []byte) (Segment, error) {
	if len(raw) == 0 {
		return Segment{}, errors.New("segment raw bytes must not be empty")
	}

	rawSum := sha256.Sum256(raw)

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	// Pin ModTime to zero so gzip output and compressed_sha256 are deterministic.
	zw.ModTime = time.Time{}
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return Segment{}, fmt.Errorf("gzip segment: %w", err)
	}
	if err := zw.Close(); err != nil {
		return Segment{}, fmt.Errorf("close gzip segment: %w", err)
	}

	compressed := gzBuf.Bytes()
	compSum := sha256.Sum256(compressed)

	return Segment{
		Harness:            meta.Harness,
		NativeRelativePath: meta.NativeRelativePath,
		ArtifactKind:       meta.ArtifactKind,
		Generation:         meta.Generation,
		ByteStart:          meta.ByteStart,
		ByteEnd:            meta.ByteStart + int64(len(raw)),
		RawSize:            int64(len(raw)),
		RawSHA256:          hex.EncodeToString(rawSum[:]),
		Compressed:         compressed,
		CompressedSize:     int64(len(compressed)),
		CompressedSHA256:   hex.EncodeToString(compSum[:]),
		ContentEncoding:    ContentEncodingGzip,
		IncompleteTail:     meta.IncompleteTail,
		SessionID:          meta.SessionID,
		OriginHubID:        meta.OriginHubID,
	}, nil
}
