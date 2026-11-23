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

package workstationsmoke

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const stubCurlScript = `#!/usr/bin/env bash
set -euo pipefail
pid_file="$HOME/.scion/server.pid"
if [ ! -f "$pid_file" ]; then
  exit 22
fi
pid="$(cat "$pid_file" 2>/dev/null || true)"
if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
  exit 22
fi
printf '{"status":"healthy","components":{"hub":{"status":"healthy"}}}\n'
`

const stubDockerScript = `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  info) exit 0 ;;
  *) exit 0 ;;
esac
`

// stubScionScript models the CLI contract exercised by hack/workstation-smoke.sh,
// including cmd/project_use_select.go's exclusion of the hub's built-in global
// project from 'scion use <ref>'.
const stubScionScript = `#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"$SMOKE_CALL_LOG"

global=0
yes=0
machine=0
format=""
proj_name=""
pos=()

while [ $# -gt 0 ]; do
  case "$1" in
    --global) global=1; shift ;;
    -y|--yes) yes=1; shift ;;
    --machine) machine=1; shift ;;
    --format) format="$2"; shift 2 ;;
    --name) proj_name="$2"; shift 2 ;;
    *) pos+=("$1"); shift ;;
  esac
done

cmd="${pos[0]:-}"
sub="${pos[1]:-}"
sub2="${pos[2]:-}"

case "$cmd" in
  init)
    if [ "$machine" = 1 ]; then
      mkdir -p "$HOME/.scion"
      cat >"$HOME/.scion/settings.yaml" <<'EOF'
profiles:
  local:
    runtime: podman  # Auto-detected
EOF
      printf '{}\n'
      exit 0
    fi
    mkdir -p .scion
    : >.scion/settings.yaml
    printf '{}\n'
    ;;

  config)
    case "$sub" in
      get)
        [ "$sub2" = "profiles.local.runtime" ] || { echo "unexpected config get: $sub2" >&2; exit 1; }
        sed -n 's/^    runtime: \([^ ]*\).*/\1/p' "$HOME/.scion/settings.yaml"
        ;;
      set)
        [ "$sub2" = "image_registry" ] || { echo "unexpected config set: $sub2" >&2; exit 1; }
        mkdir -p "$HOME/.scion"
        printf 'image_registry: %s\n' "${pos[3]:-}" >"$HOME/.scion/settings.yaml"
        ;;
      *)
        echo "unexpected config subcommand: $sub" >&2
        exit 1
        ;;
    esac
    ;;

  server)
    case "$sub" in
      start)
        mkdir -p "$HOME/.scion"
        sleep 120 >/dev/null 2>&1 </dev/null &
        printf '%s\n' "$!" >"$HOME/.scion/server.pid"
        printf 'stub server started\n' >"$HOME/.scion/server.log"
        : >"$HOME/.scion/hub-registered"
        cat <<'EOF'
Registered hub 'local' (http://127.0.0.1:8080); rename with 'scion hub rename'.
Web UI:  http://127.0.0.1:8080/onboarding
EOF
        ;;
      stop)
        if [ -f "$HOME/.scion/server.pid" ]; then
          pid="$(cat "$HOME/.scion/server.pid" 2>/dev/null || true)"
          if [ -n "$pid" ]; then
            kill "$pid" 2>/dev/null || true
            for ((i = 0; i < 50; i++)); do
              kill -0 "$pid" 2>/dev/null || break
              sleep 0.02
            done
          fi
          rm -f "$HOME/.scion/server.pid"
        fi
        ;;
      *)
        echo "unexpected server subcommand: $sub" >&2
        exit 1
        ;;
    esac
    ;;

  hub)
    case "$sub" in
      link)
        if [ ! -f "$HOME/.scion/hub-registered" ]; then
          echo "Error: No hub selected. Add one with 'scion hub add <name> <url>', start a local hub with 'scion server start', or pick one with 'scion use --hub <name>'." >&2
          exit 1
        fi
        if [ "$global" = 1 ]; then
          if [ "$yes" != 1 ]; then
            echo "Continue with linking?"
            exit 1
          fi
          printf '00000000-0000-0000-0000-000000000001\n' >"$HOME/.scion/global-linked"
          cat <<'EOF'
Linking project 'global' to the local hub at http://127.0.0.1:8080
Project 'global' is now linked to the Hub.
EOF
        else
          printf '00000000-0000-0000-0000-000000000010\n' >.scion/linked
          printf '00000000-0000-0000-0000-000000000010:repo\n' >>"$HOME/.scion/hub-projects"
          cat <<'EOF'
Linking project 'repo' to the local hub at http://127.0.0.1:8080
Project 'repo' is now linked to the Hub.
EOF
        fi
        ;;
      status)
        [ -f "$HOME/.scion/server.pid" ] || { echo "hub unreachable" >&2; exit 1; }
        if [ "$global" = 1 ]; then
          if [ -f "$HOME/.scion/global-linked" ]; then
            printf '{"connected": true, "linked": true, "hubProjectId": "00000000-0000-0000-0000-000000000001"}\n'
          else
            printf '{"connected": true, "linked": false}\n'
          fi
        else
          if [ -f .scion/linked ]; then
            id="$(cat .scion/linked)"
            printf '{"connected": true, "linked": true, "hubProjectId": "%s"}\n' "$id"
          else
            printf '{"connected": true, "linked": false}\n'
          fi
        fi
        ;;
      projects)
        case "$sub2" in
          info)
            target_id="${pos[3]:-}"
            if [ "$target_id" = "00000000-0000-0000-0000-000000000001" ]; then
              cat <<'EOF'
{
  "id": "00000000-0000-0000-0000-000000000001",
  "slug": "global"
}
EOF
              exit 0
            fi
            slug=""
            if [ -f "$HOME/.scion/hub-projects" ]; then
              slug="$(sed -n "s/^${target_id}:\(.*\)$/\1/p" "$HOME/.scion/hub-projects" | head -n 1)"
            fi
            [ -n "$slug" ] || { echo "project not found: $target_id" >&2; exit 1; }
            cat <<EOF
{
  "id": "$target_id",
  "slug": "$slug"
}
EOF
            ;;
          create)
            [ -n "$proj_name" ] || { echo "--name is required" >&2; exit 1; }
            id="00000000-0000-0000-0000-000000000020"
            printf '%s:%s\n' "$id" "$proj_name" >>"$HOME/.scion/hub-projects"
            cat <<EOF
{
  "id": "$id",
  "name": "$proj_name",
  "slug": "$proj_name",
  "workspaceMode": ""
}
EOF
            ;;
          *)
            echo "unexpected hub projects subcommand: $sub2" >&2
            exit 1
            ;;
        esac
        ;;
      *)
        echo "unexpected hub subcommand: $sub" >&2
        exit 1
        ;;
    esac
    ;;

  list)
    if [ "$global" != 1 ] && [ -d .scion ] && [ ! -f .scion/linked ]; then
      printf '00000000-0000-0000-0000-000000000011\n' >.scion/linked
      printf '00000000-0000-0000-0000-000000000011:repo2\n' >>"$HOME/.scion/hub-projects"
      echo "Linking project 'repo2' to the local hub at http://127.0.0.1:8080" >&2
    elif [ "$global" != 1 ] && [ ! -d .scion ] && [ ! -f "$HOME/.scion/context-project" ]; then
      echo "Error: no project selected" >&2
      exit 1
    fi
    echo "Using hub: http://127.0.0.1:8080" >&2
    if [ "$format" = "json" ]; then
      printf '[]\n'
    fi
    ;;

  use)
    if [ -z "$sub" ]; then
      if [ "$format" = "json" ]; then
        if [ -f "$HOME/.scion/context-project" ]; then
          printf '{"effective":{"project":{"source":"context"}}}\n'
        else
          printf '{"effective":{"project":{"source":"none"}}}\n'
        fi
      fi
      exit 0
    fi
    # Mirror cmd/project_use_select.go:isHubGlobalProject — a hub's built-in
    # global project is excluded from 'scion use <project>'.
    if [ "$sub" = "global" ] || [ "$sub" = "00000000-0000-0000-0000-000000000001" ]; then
      echo "Error: project '$sub' not found on hubs: local" >&2
      exit 1
    fi
    matched_slug=""
    if [ -f "$HOME/.scion/hub-projects" ]; then
      while IFS=':' read -r pid pslug; do
        if [ "$sub" = "$pid" ] || [ "$sub" = "$pslug" ]; then
          matched_slug="$pslug"
          break
        fi
      done <"$HOME/.scion/hub-projects"
    fi
    if [ -z "$matched_slug" ]; then
      echo "Error: project '$sub' not found on hubs: local" >&2
      exit 1
    fi
    printf '%s\n' "$matched_slug" >"$HOME/.scion/context-project"
    printf "Now using project '%s' on hub 'local' (http://127.0.0.1:8080).\n" "$matched_slug"
    ;;

  *)
    echo "unexpected scion command: $cmd" >&2
    exit 1
    ;;
esac
`

func setupStubEnv(t *testing.T, withDocker bool) (scriptPath, scionBin, binDir, callLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hack/workstation-smoke.sh requires bash and Unix utilities")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	scriptPath = filepath.Join(repoRoot, "hack", "workstation-smoke.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("locate workstation-smoke.sh: %v", err)
	}

	tmp := t.TempDir()
	binDir = filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	// Symlink only the standard tools workstation-smoke.sh and the stubs use,
	// excluding docker and podman even when the host has them installed.
	tools := []string{
		"basename", "bash", "cat", "dirname", "env", "git", "grep",
		"head", "kill", "ls", "mkdir", "mktemp", "pwd", "rm",
		"sed", "sleep", "tail", "tr",
	}
	for _, name := range tools {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("required tool %q not found on PATH: %v", name, err)
		}
		if err := os.Symlink(target, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}

	writeExec := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	writeExec(filepath.Join(binDir, "curl"), stubCurlScript)
	if withDocker {
		writeExec(filepath.Join(binDir, "docker"), stubDockerScript)
	}

	scionBin = filepath.Join(tmp, "scion")
	writeExec(scionBin, stubScionScript)
	callLog = filepath.Join(tmp, "calls.log")
	if err := os.WriteFile(callLog, nil, 0o644); err != nil {
		t.Fatalf("create call log: %v", err)
	}
	return scriptPath, scionBin, binDir, callLog
}

func runSmokeScript(t *testing.T, withDocker bool) (output string, calls []string) {
	t.Helper()
	scriptPath, scionBin, binDir, callLog := setupStubEnv(t, withDocker)
	workTmp := t.TempDir()

	cmd := exec.Command(filepath.Join(binDir, "bash"), scriptPath, scionBin)
	cmd.Env = []string{
		"PATH=" + binDir,
		"HOME=" + filepath.Join(workTmp, "caller-home"),
		"TMPDIR=" + workTmp,
		"SMOKE_CALL_LOG=" + callLog,
		"LC_ALL=C",
	}
	outBytes, err := cmd.CombinedOutput()
	output = string(outBytes)
	if err != nil {
		t.Fatalf("workstation-smoke.sh (withDocker=%v) failed: %v\nOutput:\n%s", withDocker, err, output)
	}

	rawCalls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(rawCalls)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return output, calls
}

func TestWorkstationSmoke_NoRuntimeFallback(t *testing.T) {
	out, calls := runSmokeScript(t, false)

	for _, want := range []string{
		"==> no container runtime found: skipping scion init, using the global project",
		"==> 5b. skipped: no container runtime for 'scion init'",
		"==> 5c. outside any project, scion use selects the project and scion list targets it",
		"==> workstation smoke test passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}

	var sawCreate, sawUseSlug, sawUseUUID bool
	for _, c := range calls {
		if c == "use global" || c == "use 00000000-0000-0000-0000-000000000001" {
			t.Errorf("step 5c attempted to select the built-in global project: %q", c)
		}
		if strings.Contains(c, "hub projects create") {
			sawCreate = true
		}
		if c == "use smoke" {
			sawUseSlug = true
		}
		if c == "use 00000000-0000-0000-0000-000000000020" {
			sawUseUUID = true
		}
	}
	if !sawCreate || !sawUseSlug || !sawUseUUID {
		t.Errorf("expected step 5c to create and select a hub-managed project by slug and UUID (create=%v, slug=%v, uuid=%v); calls:\n%s",
			sawCreate, sawUseSlug, sawUseUUID, strings.Join(calls, "\n"))
	}
}

func TestWorkstationSmoke_WithDockerRuntime(t *testing.T) {
	out, calls := runSmokeScript(t, true)

	for _, want := range []string{
		"==> scion init --machine && scion init",
		"==> local profile runtime: docker",
		"==> 5b. scion list in a never-linked project links it to the local hub",
		"==> 5c. outside any project, scion use selects the project and scion list targets it",
		"==> workstation smoke test passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}

	var sawUseSlug, sawUseUUID bool
	for _, c := range calls {
		if strings.Contains(c, "hub projects create") {
			t.Errorf("did not expect hub projects create when a runtime is available: %q", c)
		}
		if c == "use repo" {
			sawUseSlug = true
		}
		if c == "use 00000000-0000-0000-0000-000000000010" {
			sawUseUUID = true
		}
	}
	if !sawUseSlug || !sawUseUUID {
		t.Errorf("expected step 5c to select the linked project by slug and UUID (slug=%v, uuid=%v); calls:\n%s",
			sawUseSlug, sawUseUUID, strings.Join(calls, "\n"))
	}
}
