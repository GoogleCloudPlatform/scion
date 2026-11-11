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

// Package homeprep prepares a persistent agent home on an NFS export from
// inside the agent's pod: it creates the agent's home directory (leaf),
// decides how to fill the home at each start (prepare), and marks a seeded
// home (mark-seeded). Every file operation stays beneath the home (or the
// agent directory, for leaf) and never follows a symbolic link.
package homeprep

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FeatureToken is the capability "sciontool version --features" lists when
// this package's contract is complete: the leaf, prepare and mark-seeded
// commands with the sentinel, mode file and link result formats below.
const FeatureToken = "home-v1"

// Reserved names. Entries whose name starts with ReservedPrefix belong to
// scion: the sentinel, its temporary files and the write probe.
const (
	ReservedPrefix = ".scion-home-"
	SentinelName   = ".scion-home-seed.json"
	sentinelTmp    = ".scion-home-seed."
	probePrefix    = ".scion-home-probe."
	// LinksRecordPath records, inside the home, the links the last start
	// placed.
	LinksRecordPath = ".scion/.home-links.json"
	// hooksDir is cleared before each seed-over transfer, so hook scripts
	// that left the broker's copy of the home do not run.
	hooksDir = ".scion/hooks"
)

// Files written to the pod's memory directory for the broker.
const (
	ModeFileName        = "home-mode.json"
	LinksResultFileName = "home-links-result.json"
)

// Sentinel states.
const (
	StateSeeding = "seeding"
	StateSeeded  = "seeded"
)

// Skeleton results.
const (
	SkeletonCopied  = "copied"
	SkeletonSkipped = "skipped"
	SkeletonNone    = "none"
)

// Modes prepare chooses.
const (
	// ModeSeed fills an empty (or interrupted) home: the image skeleton,
	// then the broker's full transfer, then mark-seeded.
	ModeSeed = "seed"
	// ModeSeedOver transfers the broker's copy over an already seeded home.
	ModeSeedOver = "seed-over"
)

// Error classes, printed as the first word of a failure message so the
// broker can report them.
const (
	ErrClassUnavailable = "home_storage_unavailable"
	ErrClassPrepare     = "home_prepare_failed"
	ErrClassSeed        = "home_seed_failed"
	ErrClassLeafFailed  = "home_leaf_failed"
	ErrClassLeafMatch   = "home_leaf_mismatch"
)

// ClassError is a failure with an error class.
type ClassError struct {
	Class string
	Msg   string
}

func (e *ClassError) Error() string { return e.Class + ": " + e.Msg }

func classErr(class, format string, args ...any) error {
	return &ClassError{Class: class, Msg: fmt.Sprintf(format, args...)}
}

// Sentinel is the content of SentinelName. Manifest fields are reserved
// for a later version of the contract and are empty here.
type Sentinel struct {
	Version        int             `json:"version"`
	AgentID        string          `json:"agent_id"`
	State          string          `json:"state"`
	ManifestDigest string          `json:"manifest_digest,omitempty"`
	Manifest       json.RawMessage `json:"manifest,omitempty"`
	StartID        string          `json:"start_id"`
	LaunchID       string          `json:"launch_id,omitempty"`
	UID            int             `json:"uid"`
	GID            int             `json:"gid"`
	Skeleton       string          `json:"skeleton,omitempty"`
	SeededAt       string          `json:"seeded_at,omitempty"`
	SeededBy       string          `json:"seeded_by,omitempty"`
}

// ModeFile is the content of ModeFileName.
type ModeFile struct {
	Mode    string `json:"mode"`
	StartID string `json:"start_id"`
}

// SentinelRead is the outcome of reading the sentinel: Sentinel is set when
// it was read and parsed, NotExist when it does not exist, and Err for any
// other failure (including a symbolic link, bad JSON or an unknown
// version).
type SentinelRead struct {
	Sentinel *Sentinel
	NotExist bool
	Err      error
}

// parseSentinel parses sentinel data and checks the version and state.
func parseSentinel(data []byte) (*Sentinel, error) {
	var s Sentinel
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid sentinel: %w", err)
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("unknown sentinel version %d", s.Version)
	}
	if s.State != StateSeeding && s.State != StateSeeded {
		return nil, fmt.Errorf("unknown sentinel state %q", s.State)
	}
	return &s, nil
}

// Decision is what prepare does with the home.
type Decision struct {
	Mode string
	// CleanInterrupted is set for an interrupted seed: every entry except
	// the sentinel is removed before seeding again.
	CleanInterrupted bool
	// RemoveReserved lists reserved leftovers removed before a first seed.
	RemoveReserved []string
}

// Decide returns what prepare does, from the sentinel read result, this
// agent's ID, the current uid and the names in the home. It fails closed:
// anything but a missing sentinel on an otherwise empty home, or a
// sentinel of this agent with this uid, is an error and changes nothing.
func Decide(read SentinelRead, agentID string, uid int, names []string) (Decision, error) {
	switch {
	case read.Err != nil:
		return Decision{}, classErr(ErrClassPrepare, "cannot read the home sentinel: %v", read.Err)
	case read.NotExist:
		var reserved, other []string
		for _, n := range names {
			if strings.HasPrefix(n, ReservedPrefix) {
				reserved = append(reserved, n)
			} else {
				other = append(other, n)
			}
		}
		if len(other) > 0 {
			return Decision{}, classErr(ErrClassPrepare, "home has content but no sentinel (%d entries, for example %q)", len(other), other[0])
		}
		return Decision{Mode: ModeSeed, RemoveReserved: reserved}, nil
	}
	s := read.Sentinel
	if s == nil {
		return Decision{}, classErr(ErrClassPrepare, "no sentinel result")
	}
	if s.AgentID != agentID {
		return Decision{}, classErr(ErrClassPrepare, "the home belongs to agent %q, not %q", s.AgentID, agentID)
	}
	if s.UID != uid {
		return Decision{}, classErr(ErrClassPrepare, "the home was seeded by uid %d, this start runs as uid %d", s.UID, uid)
	}
	if s.State == StateSeeding {
		return Decision{Mode: ModeSeed, CleanInterrupted: true}, nil
	}
	return Decision{Mode: ModeSeedOver}, nil
}
