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

package util

import "time"

// PinProcessUTC pins the process's local timezone to UTC by setting
// time.Local = time.UTC. This makes every bare time.Now(), ent's
// Default(time.Now), slog's timestamps, and anything else that formats or
// compares using the local zone behave identically regardless of the host's
// TZ environment variable or /etc/localtime.
//
// Call this as the first statement of server and broker entry points (and of
// offline store-writing subcommands), before any other work — in particular
// before any cron spec is parsed, since robfig/cron captures time.Local at
// parse time. Do not call it from a PersistentPreRun on the root/server
// Cobra commands, and never call it from a CLI command: the scion binary is
// shared with the CLI, which must keep using the host's local zone.
func PinProcessUTC() {
	time.Local = time.UTC
}
