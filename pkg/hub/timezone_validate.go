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
	"errors"
	"strings"
	"time"
)

// nonPortableTimezoneNames are tzdata entries time.LoadLocation accepts
// (Go's embedded zoneinfo ships the actual files) but that do not name a
// portable, specific IANA zone: "Local" is the host process's ambient zone,
// "localtime" and "posixrules" are tzdata's own implementation files (not
// geographic zones), and "Factory" is tzdata's explicit
// "deliberately uninformative" placeholder.
//
// Shared by every timezone-name validator in this package: the hub-wide
// agent_defaults.default_timezone validator (admin_settings.go/
// admin_settings_db.go), the per-user display-timezone preference
// validator (handlers_users_core.go) and the metrics dashboard's tz
// parameter resolver (resolveDashboardTimeZone, metrics_dashboard.go) all
// use this one list, through validateIANATimezone, so they cannot drift
// apart from each other.
var nonPortableTimezoneNames = map[string]bool{
	"Local":      true,
	"localtime":  true,
	"posixrules": true,
	"Factory":    true,
}

// errNonPortableTimezone is validateIANATimezone's sentinel for a
// nonPortableTimezoneNames rejection, distinct from a plain
// time.LoadLocation failure, so a caller can give a more specific message
// for the denylist case without re-checking the map itself. Match it with
// errors.Is.
var errNonPortableTimezone = errors.New("not an IANA time zone name")

// validateIANATimezone reports whether tz is a real, portable IANA time
// zone name: rejects nonPortableTimezoneNames and any "right/" or "posix/"
// prefixed name (both errNonPortableTimezone, since time.LoadLocation
// itself accepts all of them on a host whose zoneinfo tree has those
// entries) and otherwise defers to time.LoadLocation.
//
// "right/" and "posix/" are whole-tree duplicates of the same zone data
// under a path prefix (right/ with leap seconds baked in, posix/ without),
// not zone names themselves, and whether they resolve is host-dependent:
// time.LoadLocation reads the host's zoneinfo directory, so
// LoadLocation("right/Asia/Tokyo") or LoadLocation("posix/Asia/Tokyo")
// succeeds wherever that tree exists and fails where it doesn't — the same
// non-portability "Local" and "posixrules" are already rejected for.
// "Asia/Tokyo" without the prefix is unaffected and still accepted.
//
// Does not special-case the empty string: whether "" is valid, and what it
// means (Auto for the per-user display preference, UTC for the hub-wide
// default), is each caller's own field semantic, not a fact about time zone
// names in general. Callers check that before calling this.
func validateIANATimezone(tz string) error {
	if nonPortableTimezoneNames[tz] {
		return errNonPortableTimezone
	}
	if strings.HasPrefix(tz, "right/") || strings.HasPrefix(tz, "posix/") {
		return errNonPortableTimezone
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return err
	}
	return nil
}
