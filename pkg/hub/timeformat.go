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

import "time"

// formatUTCTimestamp converts t to UTC before formatting it with layout.
//
// This exists so every "format a time as a wire timestamp" call site goes
// through one place: without it, it is easy to add .Format(...) and forget
// the leading .UTC(), which silently mislabels a local wall clock with a
// trailing "Z" or offset that does not match it (tz-refactor task 1, design §2.2).
// Both call sites it replaces — the GitHub App token expiry
// (handlers_github_app_webhook.go) and the invite audit-log expires_at
// (admin_invites.go) — used to call .Format directly, one of them (the
// GitHub token) with no .UTC() at all.
func formatUTCTimestamp(t time.Time, layout string) string {
	return t.UTC().Format(layout)
}
