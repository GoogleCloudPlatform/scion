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
	"time"
)

// nonPortableTimezoneNames are tzdata entries time.LoadLocation accepts
// (Go's embedded zoneinfo ships the actual files) but that do not name a
// portable, specific IANA zone: "Local" is the host process's ambient zone,
// "localtime" and "posixrules" are tzdata's own implementation files (not
// geographic zones), and "Factory" is tzdata's explicit
// "deliberately uninformative" placeholder.
//
// Shared by every timezone-name validator in this package — originally the
// hub-wide agent_defaults.default_timezone validator
// (admin_settings.go/admin_settings_db.go) and the per-user display-timezone
// preference validator (handlers_users_core.go) each declared their own
// identical copy, which built fine independently but broke the package once
// both landed (tz-refactor task 12 review round 2, R2-1: duplicate
// package-level declaration). One list, used by both, so they cannot drift
// again.
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
// zone name: rejects nonPortableTimezoneNames (errNonPortableTimezone, since
// time.LoadLocation itself accepts all four) and otherwise defers to
// time.LoadLocation.
//
// Does not special-case the empty string: whether "" is valid, and what it
// means (Auto for the per-user display preference, UTC for the hub-wide
// default), is each caller's own field semantic, not a fact about time zone
// names in general. Callers check that before calling this.
func validateIANATimezone(tz string) error {
	if nonPortableTimezoneNames[tz] {
		return errNonPortableTimezone
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return err
	}
	return nil
}
