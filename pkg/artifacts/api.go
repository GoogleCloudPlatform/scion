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
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RefScheme prefixes an artifact reference: scion://artifact/<id>[@<seq>].
const RefScheme = "scion://artifact/"

// FormatRef returns the reference string of an artifact, pinned to version
// seq when seq > 0.
func FormatRef(id string, seq int) string {
	if seq > 0 {
		return RefScheme + id + "@" + strconv.Itoa(seq)
	}
	return RefScheme + id
}

// ErrBadRef is returned by ParseRef for a malformed reference.
var ErrBadRef = errors.New("artifact reference must be scion://artifact/<id>[@<seq>] or a bare id")

// ParseRef parses scion://artifact/<id>[@<seq>] or a bare <id>[@<seq>].
// seq is 0 when the reference names the current version. The id must be a
// UUID in its canonical 36-character form (any case), the form the
// service assigns; the returned id is lowercase.
func ParseRef(ref string) (id string, seq int, err error) {
	s := strings.TrimSpace(ref)
	s = strings.TrimPrefix(s, RefScheme)
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		n, ok := parseSeq(s[at+1:])
		if !ok {
			return "", 0, ErrBadRef
		}
		s, seq = s[:at], n
	}
	u, perr := uuid.Parse(s)
	if perr != nil || len(s) != 36 {
		return "", 0, ErrBadRef
	}
	return u.String(), seq, nil
}

// ArtifactResponse is the body of GET /api/v1/artifacts/{id} and of a
// successful publish.
type ArtifactResponse struct {
	Artifact ArtifactInfo `json:"artifact"`
	// Version is the current version, or the version a write created or
	// finalized.
	Version *VersionInfo `json:"version,omitempty"`
	// Warnings are notes about the write that did not fail it.
	Warnings []string `json:"warnings,omitempty"`
}

// CreateVersionRequest is the body of POST /api/v1/artifacts (create an
// artifact with a pending first version, or append to the caller's
// artifact with the same key) and of POST /api/v1/artifacts/{id}/versions
// (append a pending version). Title, Key and Scope apply to the first form
// only.
type CreateVersionRequest struct {
	Title string `json:"title,omitempty"`
	Key   string `json:"key,omitempty"`
	Scope string `json:"scope,omitempty"`
	// Kind is the version kind; "" means publish.
	Kind  string `json:"kind,omitempty"`
	Entry string `json:"entry"`
	Note  string `json:"note,omitempty"`
	// Files is the version's manifest.
	Files []ManifestFile `json:"files"`
}

// FinalizeRequest is the optional JSON body of a finalize request.
type FinalizeRequest struct {
	// Base is the version a review was started from. It is required to
	// finalize a review and ignored otherwise.
	Base int `json:"base,omitempty"`
}

// ManifestFile is one file of a version manifest.
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// MediaType is optional; the service derives one from the path and the
	// bytes when it is empty or generic.
	MediaType string `json:"mediaType,omitempty"`
}

// PendingVersionResponse answers a request that created a pending
// version.
type PendingVersionResponse struct {
	Artifact ArtifactInfo `json:"artifact"`
	Version  *VersionInfo `json:"version"`
	Upload   UploadInfo   `json:"upload"`
}

// UploadInfo tells the publisher what to upload before finalizing.
type UploadInfo struct {
	// Required lists the manifest paths whose bytes must be uploaded with
	// PUT .../versions/{seq}/files/{path}. A file identical to one of the
	// artifact's current version needs no upload.
	Required []string `json:"required"`
}

// VersionListResponse is the body of GET /api/v1/artifacts/{id}/versions:
// the ready versions, newest first, without their files.
type VersionListResponse struct {
	Versions []VersionInfo `json:"versions"`
	// NextBefore, when set, is the before= value of the next page.
	NextBefore int `json:"nextBefore,omitempty"`
}

// ArtifactInfo describes an artifact.
type ArtifactInfo struct {
	ID         string    `json:"id"`
	Ref        string    `json:"ref"`
	ScopeKind  string    `json:"scopeKind"`
	ScopeRef   string    `json:"scopeRef"`
	OwnerKind  string    `json:"ownerKind"`
	OwnerRef   string    `json:"ownerRef"`
	Key        string    `json:"key,omitempty"`
	Title      string    `json:"title"`
	CurrentSeq int       `json:"currentSeq"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// VersionInfo describes one version and its files.
type VersionInfo struct {
	Seq           int        `json:"seq"`
	Ref           string     `json:"ref"`
	Kind          string     `json:"kind"`
	EntryPath     string     `json:"entryPath"`
	Note          string     `json:"note,omitempty"`
	TotalBytes    int64      `json:"totalBytes"`
	FileCount     int        `json:"fileCount"`
	CreatedByKind string     `json:"createdByKind,omitempty"`
	CreatedByRef  string     `json:"createdByRef,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	State         string     `json:"state"`
	Files         []FileInfo `json:"files,omitempty"`
}

// FileInfo describes one file of a version.
type FileInfo struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	// Origin is "remote" for an image the hub fetched at publish time and
	// omitted for uploaded files. SourceURL is the URL it was fetched from
	// and FetchStatus is "ok" or "failed".
	Origin      string `json:"origin,omitempty"`
	SourceURL   string `json:"sourceUrl,omitempty"`
	FetchStatus string `json:"fetchStatus,omitempty"`
}

func artifactInfo(a *Artifact) ArtifactInfo {
	return ArtifactInfo{
		ID: a.ID, Ref: FormatRef(a.ID, 0), ScopeKind: a.ScopeKind, ScopeRef: a.ScopeRef,
		OwnerKind: a.OwnerKind, OwnerRef: a.OwnerRef, Key: a.Key, Title: a.Title,
		CurrentSeq: a.CurrentSeq, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

func versionInfo(v *Version, files []File) *VersionInfo {
	out := &VersionInfo{
		Seq: v.Seq, Ref: FormatRef(v.ArtifactID, v.Seq), Kind: v.Kind, EntryPath: v.EntryPath, Note: v.Note,
		TotalBytes: v.TotalBytes, FileCount: v.FileCount, CreatedByKind: v.CreatedByKind,
		CreatedByRef: v.CreatedByRef, CreatedAt: v.CreatedAt, State: v.State, Files: []FileInfo{},
	}
	for _, f := range files {
		fi := FileInfo{Path: f.Path, Size: f.Size, SHA256: f.SHA256, MediaType: f.MediaType}
		if f.Origin == FileOriginRemote {
			fi.Origin, fi.SourceURL, fi.FetchStatus = f.Origin, f.SourceURL, f.FetchStatus
		}
		out.Files = append(out.Files, fi)
	}
	return out
}
