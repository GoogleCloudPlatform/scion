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

package update

import (
	"regexp"
	"strconv"
	"time"

	"golang.org/x/mod/semver"
)

// Class describes how a target release relates to the running version.
type Class string

const (
	// ClassSame means the target is the running version.
	ClassSame Class = "same"
	// ClassUpgrade means the target is newer, in the same channel.
	ClassUpgrade Class = "upgrade"
	// ClassSwitchNewer means the target is in another channel (or the running
	// binary is a git build) and was published after the running version.
	ClassSwitchNewer Class = "switch-newer"
	// ClassSwitchOlder means the target is in another channel (or the running
	// binary is a git build) and was not published after the running version.
	ClassSwitchOlder Class = "switch-older"
	// ClassDowngrade means the target is older, in the same channel.
	ClassDowngrade Class = "downgrade"
	// ClassUnknown means the order cannot be decided from the inputs: the
	// target is not a release tag, or a cross-channel comparison lacks a date
	// for one side.
	ClassUnknown Class = "unknown"
)

// IsDowngrade reports whether c installs an older release than the running
// one (downgrade or switch-older).
func (c Class) IsDowngrade() bool {
	return c == ClassDowngrade || c == ClassSwitchOlder
}

// Ref identifies a version for Compare. Date is when that version was
// published (for a release) or built (for a git build); the zero value means
// unknown. Nightly tags carry their own date, used when Date is zero.
type Ref struct {
	Version string
	Date    time.Time
}

// nightlyRE matches nightly-YYYYMMDD with an optional numeric ".N" suffix
// for a second build on the same day.
var nightlyRE = regexp.MustCompile(`^nightly-(\d{8})(?:\.(\d+))?$`)

// ParseNightly returns the build date and same-day sequence number (0 when
// absent) of a nightly tag. ok is false when tag is not a well-formed nightly.
func ParseNightly(tag string) (date time.Time, seq int, ok bool) {
	m := nightlyRE.FindStringSubmatch(tag)
	if m == nil {
		return time.Time{}, 0, false
	}
	d, err := time.Parse("20060102", m[1])
	if err != nil {
		return time.Time{}, 0, false
	}
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return time.Time{}, 0, false
		}
		seq = n
	}
	return d, seq, true
}

// IsReleaseTag reports whether tag is a well-formed release tag of a known
// channel: a nightly-YYYYMMDD[.N] tag or a semantic version (with or without
// a "v" prefix). Release tags are safe to use as a URL path element.
func IsReleaseTag(tag string) bool {
	switch DetectChannel(tag) {
	case "nightly":
		_, _, ok := ParseNightly(tag)
		return ok
	case "preview", "stable":
		// semver.IsValid allows build metadata ("+..."), which is not used
		// in release tags and is not path-safe; refuse it.
		return semver.Build(withVersionPrefix(tag)) == ""
	default:
		return false
	}
}

// Compare classifies target against the running version current, using only
// what the version strings carry (nightly tags carry a date). A cross-channel
// comparison involving a preview or stable release returns ClassUnknown; use
// CompareRefs with publish dates for those.
func Compare(current, target string) Class {
	return CompareRefs(Ref{Version: current}, Ref{Version: target})
}

// CompareRefs classifies target against the running version current.
//
//   - Same channel: nightlies compare by date then sequence; preview and
//     stable compare as semantic versions.
//   - Different channel, or a running git build (no release version): the
//     dates decide, newer → switch-newer, otherwise switch-older. Without a
//     date on either side the result is ClassUnknown.
func CompareRefs(current, target Ref) Class {
	if !IsReleaseTag(target.Version) {
		return ClassUnknown
	}
	curChannel := ""
	if IsReleaseTag(current.Version) {
		curChannel = DetectChannel(current.Version)
	}
	tgtChannel := DetectChannel(target.Version)

	if curChannel != "" && curChannel == tgtChannel {
		switch c := compareSameChannel(current.Version, target.Version, tgtChannel); {
		case c == 0:
			return ClassSame
		case c < 0:
			return ClassUpgrade
		default:
			return ClassDowngrade
		}
	}

	curDate, tgtDate := refDate(current), refDate(target)
	if curDate.IsZero() || tgtDate.IsZero() {
		return ClassUnknown
	}
	if tgtDate.After(curDate) {
		return ClassSwitchNewer
	}
	return ClassSwitchOlder
}

// compareSameChannel returns -1, 0 or +1 as a is older than, equal to or
// newer than b. Both must be well-formed tags of channel.
func compareSameChannel(a, b, channel string) int {
	if channel == "nightly" {
		da, sa, _ := ParseNightly(a)
		db, sb, _ := ParseNightly(b)
		switch {
		case da.Before(db):
			return -1
		case da.After(db):
			return 1
		case sa < sb:
			return -1
		case sa > sb:
			return 1
		default:
			return 0
		}
	}
	return semver.Compare(withVersionPrefix(a), withVersionPrefix(b))
}

func refDate(r Ref) time.Time {
	if !r.Date.IsZero() {
		return r.Date
	}
	if d, _, ok := ParseNightly(r.Version); ok {
		return d
	}
	return time.Time{}
}
