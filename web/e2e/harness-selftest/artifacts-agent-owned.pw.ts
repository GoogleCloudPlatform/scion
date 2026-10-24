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
 * Unit checks of the agent-owned artifact journey's pure parts (no hub, no
 * browser): the agent shell commands and capture readers, and the row
 * table and its oracle row map.
 */

import { expect, test } from '@playwright/test';

import {
  ARTIFACT_KEY,
  FILE_DONE,
  READY,
  REQUIRED_SCOPES,
  SHELL_COMMANDS,
  WHOAMI_DONE,
  markerSeen,
  missingScopes,
  parsePublish,
  parseScopes,
} from '../artifacts-agent-owned/agent-shell.js';
import {
  AGENT_ROW_IDS,
  ORACLE_ROW_MAP,
  ROWS,
  ROW_IDS,
  TITLE_CLEANUP,
  TITLE_JOURNEY,
  noRuntimeReason,
} from '../artifacts-agent-owned/rows.js';
import { DEVIATION, refuseRemoteTarget } from '../artifacts-agent-owned/state.js';

const ID = '5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d';

test('a marker counts only as an output line, never as the typed command', () => {
  const typed = `$ ${SHELL_COMMANDS.ready}`;
  expect(markerSeen(typed, READY)).toBe(false);
  expect(markerSeen(`${typed}\nR1B-READY\n$ `, READY)).toBe(true);
  expect(markerSeen('R1B-READY   \n', READY)).toBe(true);
  expect(markerSeen('xR1B-READY\n', READY)).toBe(false);
  expect(SHELL_COMMANDS.file).toContain(FILE_DONE.suffix);
  expect(SHELL_COMMANDS.whoami).toContain(WHOAMI_DONE.suffix);
  for (const cmd of Object.values(SHELL_COMMANDS)) {
    expect(cmd, 'one line per keys call').not.toContain('\n');
  }
});

test('parseScopes reads the last Scopes output line', () => {
  expect(parseScopes('$ scion whoami\nName: a\n')).toBeNull();
  expect(
    parseScopes(
      "$ grep '^Scopes:' who.txt\nScopes:\tproject:read, project:artifact:read, project:artifact:write\n"
    )
  ).toEqual(['project:read', 'project:artifact:read', 'project:artifact:write']);
  expect(parseScopes('Scopes:\ta\nScopes:\tb, c\n')).toEqual(['b', 'c']);
});

test('missingScopes names the artifact scopes the token lacks', () => {
  expect(missingScopes(null)).toEqual([...REQUIRED_SCOPES]);
  expect(missingScopes(['project:artifact:read'])).toEqual(['project:artifact:write']);
  expect(missingScopes(['project:artifact:write', 'project:artifact:read'])).toEqual([]);
});

test('the publish command is gated on both scopes and prints its exit status', () => {
  for (const s of REQUIRED_SCOPES) expect(SHELL_COMMANDS.publish).toContain(`grep -q '${s}'`);
  expect(SHELL_COMMANDS.publish.indexOf('grep -q')).toBeLessThan(
    SHELL_COMMANDS.publish.indexOf('scion artifact publish')
  );
  expect(SHELL_COMMANDS.publish).toContain(`--key ${ARTIFACT_KEY}`);
});

test('parsePublish reads the exit status and the reference, not the typed format', () => {
  const typed = `$ ${SHELL_COMMANDS.publish}`;
  expect(parsePublish(typed)).toEqual({ exit: null, ref: null, id: null, version: null });
  const out = `${typed}\nR1B-PUBLISH-EXIT=0\nscion://artifact/${ID}  (v1)\nhttps://h/projects/p/artifacts/${ID}\n`;
  expect(parsePublish(out)).toEqual({
    exit: 0,
    ref: `scion://artifact/${ID}`,
    id: ID,
    version: 1,
  });
  expect(parsePublish(`${typed}\nR1B-PUBLISH-EXIT=1\nError: forbidden\n`).exit).toBe(1);
});

test('the row map is complete and every row is mapped once', () => {
  expect(Object.keys(ORACLE_ROW_MAP).sort()).toEqual([...ROW_IDS].sort());
  for (const id of ROW_IDS) {
    expect([TITLE_JOURNEY, TITLE_CLEANUP]).toContain(ORACLE_ROW_MAP[id].test);
  }
  for (const id of [
    'R1B-P1',
    'R1B-P2',
    'R1B-P3',
    'R1B-P4',
    'R1B-P5',
    'R1B-P6',
    'R1B-P7',
    'R1B-P8',
  ]) {
    expect(ROWS[id]?.stop, `${id} is a STOP row`).toBe(true);
  }
  expect(ROWS['R1B-12'].graded).toBe(false);
  expect(AGENT_ROW_IDS).not.toContain('R1B-17');
  expect(AGENT_ROW_IDS).not.toContain('R1B-P1');
  expect(AGENT_ROW_IDS).toContain('R1B-P8');
  expect(noRuntimeReason('x')).toMatch(/^NOT RUN: no container runtime in this lane/);
  expect(DEVIATION).toBe('member test-mode; local issuer, no workload delegation');
});

test('a remote target is refused', () => {
  expect(() => refuseRemoteTarget({ E2E_TARGET_URL: 'https://hub.example' })).toThrow(
    /local throwaway hub/
  );
  expect(() => refuseRemoteTarget({})).not.toThrow();
});
