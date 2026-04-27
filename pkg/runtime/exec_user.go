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

package runtime

import "regexp"

// ValidExecUserName matches usernames that are safe to interpolate
// into shell command lines built by ExecAsUserCmd. Compiled once at
// package init so callers (e.g. KubernetesRuntime.Attach) avoid a
// per-invocation regexp.MustCompile, and so the broker's
// sanitizeExecUser shares one source of truth for the rule.
var ValidExecUserName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ExecAsUserCmd returns a /bin/sh command vector that runs cmd as user.
//
// If the container already runs as user (no su needed — common on GKE
// Autopilot or any image with a non-root USER directive in the Dockerfile),
// the wrapper execs cmd directly. Otherwise it falls back to
// `su - user -c cmd` for legacy images that start as root.
//
// This avoids the PAM password prompt that occurs when root tries to su
// to another user on container images whose /etc/pam.d/su lacks the
// `auth sufficient pam_rootok.so` line — the default in node:20-slim and
// other Debian-derived bases. On those images, a non-interactive
// `su - <user> -c ...` exits with "Authentication failure" and the
// caller (typically a readiness probe or PTY exec) misinterprets the
// failure as the target service not being ready.
//
// See PR #159 for the original inline fix in KubernetesRuntime.Attach
// that this helper generalizes to all su-wrapping call sites in the
// broker and runtime packages.
//
// Both branches route the cmd through a fresh `sh -c` invocation, and
// user/cmd are passed as positional shell arguments ($1/$2) rather
// than interpolated into the script body. The shell's argv machinery
// preserves them verbatim — no Go-side shell quoting, no risk of
// double-quoted expansion in the outer wrapper. The inner shell
// then parses cmd as a normal command line: leading env-var
// assignments work (e.g. "TERM=xterm-256color tmux attach-session
// -t scion"), shell metacharacters are interpreted by the inner
// shell, and $VAR references resolve against that inner shell's
// environment.
func ExecAsUserCmd(user, cmd string) []string {
	// Per `sh -c <script> <arg0> <arg1> ...`: arg0 becomes $0
	// (conventionally the script name, which surfaces in ps and
	// error messages), arg1 becomes $1, etc. We label $0 as
	// "exec-as-user" so any diagnostic that prints it is
	// self-describing.
	const script = `if [ "$(whoami)" = "$1" ]; then exec sh -c "$2"; else exec su - "$1" -c "$2"; fi`
	return []string{"sh", "-c", script, "exec-as-user", user, cmd}
}
