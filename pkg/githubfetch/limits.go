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

package githubfetch

import "time"

// Policy selects how an unsupported or special entry (submodule, symlink) is
// handled during tree validation and tarball extraction.
type Policy int

const (
	// PolicySkip skips the entry and records it in SkipCounts.
	PolicySkip Policy = iota
	// PolicyReject fails the fetch with KindUnsupportedContent.
	PolicyReject
)

// DefaultMaxNameBytes is the default maximum relative path length (in bytes)
// for a single file or directory inside a fetched folder.
const (
	DefaultMaxNameBytes = 4096

	// DefaultRawMaxFiles is the default maximum number of files in a tree for
	// which FetchFolder uses the per-file raw download strategy.
	DefaultRawMaxFiles = 64

	// DefaultRawMaxBytes is the default maximum total file bytes in a tree for
	// which FetchFolder uses the per-file raw download strategy.
	DefaultRawMaxBytes = 4 << 20 // 4 MiB

	// DefaultRawConcurrency is the default maximum number of concurrent raw
	// file downloads in FetchFolder.
	DefaultRawConcurrency = 8

	// DefaultFetchFileMaxBytes is the fallback per-file byte limit used by
	// Client.FetchFile when neither File.Size nor a caller limit is set.
	DefaultFetchFileMaxBytes = 16 << 20 // 16 MiB
)

// Limits bounds one GitHub tree listing and folder materialization.
type Limits struct {
	MaxFiles     int
	MaxNameBytes int
	MaxFileBytes int64
	MaxTotal     int64
	MaxDownload  int64
	MaxUnpacked  int64
	// RawMaxFiles is the maximum number of files eligible for the per-file raw
	// strategy in FetchFolder. Zero falls back to Client Options /
	// DefaultRawMaxFiles; a negative value disables raw and forces tarball.
	RawMaxFiles int
	// RawMaxBytes is the maximum total file bytes eligible for the per-file raw
	// strategy in FetchFolder. Zero falls back to Client Options /
	// DefaultRawMaxBytes; a negative value disables raw and forces tarball.
	RawMaxBytes int64
	// Timeout bounds the entire FetchFolder operation.
	Timeout time.Duration
	// RequestTimeout bounds one HTTP request (defaults to Timeout when <= 0).
	RequestTimeout time.Duration
	OnSubmodule    Policy
	OnSymlink      Policy
}

func (l Limits) withDefaults() Limits {
	if l.MaxNameBytes <= 0 {
		l.MaxNameBytes = DefaultMaxNameBytes
	}
	return l
}

// SkillLimits returns the limits for gh:// skill resolution and installation.
func SkillLimits() Limits {
	return Limits{
		MaxFiles:       1000,
		MaxNameBytes:   DefaultMaxNameBytes,
		MaxFileBytes:   10 << 20,
		MaxTotal:       32 << 20,
		MaxDownload:    64 << 20,
		MaxUnpacked:    512 << 20,
		Timeout:        20 * time.Second,
		RequestTimeout: 20 * time.Second,
		OnSubmodule:    PolicyReject,
		OnSymlink:      PolicySkip,
	}
}

// ImportHostedLimits returns the limits for hosted Hub imports and reimports.
func ImportHostedLimits() Limits {
	return Limits{
		MaxFiles:       2000,
		MaxNameBytes:   DefaultMaxNameBytes,
		MaxFileBytes:   16 << 20,
		MaxTotal:       32 << 20,
		MaxDownload:    64 << 20,
		MaxUnpacked:    512 << 20,
		RequestTimeout: 2 * time.Minute,
		Timeout:        5 * time.Minute,
		OnSubmodule:    PolicySkip,
		OnSymlink:      PolicySkip,
	}
}

// ImportLocalLimits returns the limits for workstation Hub and CLI imports.
func ImportLocalLimits() Limits {
	return Limits{
		MaxFiles:       20000,
		MaxNameBytes:   DefaultMaxNameBytes,
		MaxFileBytes:   64 << 20,
		MaxTotal:       256 << 20,
		MaxDownload:    1 << 30,
		MaxUnpacked:    4 << 30,
		RequestTimeout: 2 * time.Minute,
		Timeout:        10 * time.Minute,
		OnSubmodule:    PolicySkip,
		OnSymlink:      PolicySkip,
	}
}
