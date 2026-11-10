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

/**
 * The agent shell fixture (spec section 4, F3): the commands typed into the
 * credential-free agent's shell through its creator's keys operation, and
 * the pure readers of the terminal capture. No hub or browser here, so the
 * harness self-test can check them.
 *
 * Every command ends by printing a marker with printf and a %s argument
 * (`printf 'R1B-%s\n' DONE`): the typed command line shows the format, not
 * the marker, so a marker line in the capture is always output.
 */

/** The fixture file, artifact key, title and note (spec F3). */
export const FIXTURE_DIR = '/tmp/r1b';
export const FIXTURE_FILE = `${FIXTURE_DIR}/r1b-agent.md`;
export const ARTIFACT_KEY = 'r1b-agent';
export const ARTIFACT_TITLE = 'R1-B agent fixture';
export const ARTIFACT_NOTE = 'v1 from the agent';

/** The two scopes P8 requires in the agent token before publish. */
export const REQUIRED_SCOPES = ['project:artifact:read', 'project:artifact:write'] as const;

/** A marker command suffix and the line it prints. */
function marker(name: string): { suffix: string; line: string } {
  return { suffix: `printf 'R1B-%s\\n' ${name}`, line: `R1B-${name}` };
}

export const READY = marker('READY');
export const FILE_DONE = marker('FILE-DONE');
export const WHOAMI_DONE = marker('WHOAMI-DONE');

/** The shell commands, in order. Each is typed as one keys call, then Enter. */
export const SHELL_COMMANDS = {
  ready: READY.suffix,
  file:
    `mkdir -p ${FIXTURE_DIR} && printf '# R1-B agent fixture\\n\\n` +
    `Published by an agent from inside its container.\\n' > ${FIXTURE_FILE}; ${FILE_DONE.suffix}`,
  whoami:
    `scion whoami --full > ${FIXTURE_DIR}/who.txt 2>&1; ` +
    `grep '^Scopes:' ${FIXTURE_DIR}/who.txt; ${WHOAMI_DONE.suffix}`,
  // The publish is chained after a shell test of the same whoami output, so
  // a token without both scopes cannot publish even if the runner's own
  // check were skipped. The exit status printed is the chain's.
  publish:
    `grep -q '${REQUIRED_SCOPES[0]}' ${FIXTURE_DIR}/who.txt && ` +
    `grep -q '${REQUIRED_SCOPES[1]}' ${FIXTURE_DIR}/who.txt && ` +
    `scion artifact publish ${FIXTURE_FILE} --key ${ARTIFACT_KEY} --title '${ARTIFACT_TITLE}' ` +
    `--note '${ARTIFACT_NOTE}' > ${FIXTURE_DIR}/pub.txt 2>&1; ` +
    `printf 'R1B-PUBLISH-EXIT=%s\\n' $?; cat ${FIXTURE_DIR}/pub.txt`,
} as const;

/** The terminal capture command (the route `scion look` uses), lines joined. */
export const CAPTURE_COMMAND = ['tmux', 'capture-pane', '-p', '-J', '-S', '-300', '-t', 'scion'];

/** The capture's lines, trailing blanks trimmed. */
function lines(capture: string): string[] {
  return capture.split('\n').map((l) => l.replace(/\s+$/, ''));
}

/**
 * Whether the capture's last non-blank line is a bash prompt (ends in "$"
 * or "#", as bash's default prompts do): the no-auth fallback's
 * `exec bash` is up and reads input.
 */
export function shellPromptShown(capture: string): boolean {
  const last = lines(capture)
    .filter((l) => l.trim() !== '')
    .at(-1);
  return last !== undefined && /[$#]$/.test(last);
}

/** Whether the capture holds the marker as a whole output line. */
export function markerSeen(capture: string, m: { line: string }): boolean {
  return lines(capture).some((l) => l === m.line);
}

/**
 * The scopes of the last `Scopes:` output line (whoami prints
 * "Scopes:<tab>a, b"), or null when there is none.
 */
export function parseScopes(capture: string): string[] | null {
  const found = lines(capture).filter((l) => /^Scopes:\s/.test(l));
  const last = found.at(-1);
  if (last === undefined) return null;
  return last
    .replace(/^Scopes:\s*/, '')
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s !== '');
}

/** The required scopes missing from scopes (all of them for null). */
export function missingScopes(scopes: string[] | null): string[] {
  return REQUIRED_SCOPES.filter((s) => !(scopes ?? []).includes(s));
}

/** What the publish command printed: its exit status and the artifact reference. */
export interface PublishResult {
  exit: number | null;
  ref: string | null;
  id: string | null;
  version: number | null;
}

const UUID = '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}';

/** Reads the publish command's output from the capture. */
export function parsePublish(capture: string): PublishResult {
  const all = lines(capture);
  const exitLine = all.filter((l) => /^R1B-PUBLISH-EXIT=\d+$/.test(l)).at(-1);
  const refLine = all
    .filter((l) => new RegExp(`^scion://artifact/${UUID}\\s+\\(v\\d+\\)$`).test(l))
    .at(-1);
  const ref = refLine ? (/^(\S+)/.exec(refLine)?.[1] ?? null) : null;
  const version = refLine ? Number(/\(v(\d+)\)$/.exec(refLine)?.[1]) : null;
  return {
    exit: exitLine ? Number(exitLine.split('=')[1]) : null,
    ref,
    id: ref ? ref.replace('scion://artifact/', '') : null,
    version,
  };
}
