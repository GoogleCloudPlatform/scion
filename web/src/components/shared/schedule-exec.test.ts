/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Unit tests for shared scheduled-exec validation, payload redaction parsing,
 * duration formatting, and agent eligibility helpers (ptone/scion#7886).
 */

import { describe, it, expect } from 'vitest';
import {
  DEFAULT_EXEC_TIMEOUT_SEC,
  EXEC_NO_OUTPUT_NOTE,
  MAX_EXEC_ARGC,
  MAX_EXEC_ARG_BYTES,
  MAX_EXEC_ARGV_BYTES,
  MAX_EXEC_TIMEOUT_SEC,
  argvEqual,
  formatExecDuration,
  isAgentSelectableForExec,
  parseExecPayload,
  parseExecTimeoutValue,
  validateExecArgv,
  validateExecTarget,
} from './schedule-exec.js';

describe('validateExecArgv', () => {
  it('accepts valid commands including explicit empty trailing arguments', () => {
    expect(validateExecArgv(['true'])).toBeNull();
    expect(validateExecArgv(['/usr/bin/git', 'commit', '-m', ''])).toBeNull();
    expect(validateExecArgv(Array.from({ length: MAX_EXEC_ARGC }, () => 'a'))).toBeNull();
  });

  it('rejects empty argv and empty argv[0]', () => {
    expect(validateExecArgv([])).toMatch(/required/i);
    expect(validateExecArgv([''])).toMatch(/argv\[0\]/i);
    expect(validateExecArgv(['', 'arg1'])).toMatch(/argv\[0\]/i);
  });

  it('rejects more than 64 arguments', () => {
    const tooMany = Array.from({ length: MAX_EXEC_ARGC + 1 }, () => 'x');
    expect(validateExecArgv(tooMany)).toMatch(/64/);
  });

  it('rejects NUL bytes in any argument', () => {
    expect(validateExecArgv(['echo', 'bad\0arg'])).toMatch(/NUL/);
  });

  it('measures UTF-8 byte lengths for per-arg (4 KiB) and total (16 KiB) bounds', () => {
    // 2049 2-byte UTF-8 characters = 4098 bytes > 4096 even though .length < 4096.
    const multiByte = 'é'.repeat(2049);
    expect(multiByte.length).toBeLessThan(MAX_EXEC_ARG_BYTES);
    expect(validateExecArgv(['echo', multiByte])).toMatch(/4096/);

    const exactArg = 'a'.repeat(MAX_EXEC_ARG_BYTES);
    expect(validateExecArgv([exactArg])).toBeNull();

    // 4 args of 4096 bytes = 16384 (exact max: allowed); +1 byte exceeds 16384.
    expect(validateExecArgv([exactArg, exactArg, exactArg, exactArg])).toBeNull();
    expect(validateExecArgv([exactArg, exactArg, exactArg, exactArg, 'z'])).toMatch(/16384/);
    expect(MAX_EXEC_ARGV_BYTES).toBe(16384);
  });
});

describe('parseExecTimeoutValue', () => {
  it('treats blank input as 0 (use hub default 60s)', () => {
    expect(parseExecTimeoutValue('')).toEqual({ value: 0 });
    expect(parseExecTimeoutValue('   ')).toEqual({ value: 0 });
    expect(DEFAULT_EXEC_TIMEOUT_SEC).toBe(60);
  });

  it('accepts integers 1..3600 and rejects out-of-range or non-integer values', () => {
    expect(parseExecTimeoutValue('1')).toEqual({ value: 1 });
    expect(parseExecTimeoutValue('3600')).toEqual({ value: MAX_EXEC_TIMEOUT_SEC });
    expect(parseExecTimeoutValue(' 120 ')).toEqual({ value: 120 });

    for (const bad of ['0', '-1', '3601', '1.5', '60s', 'abc']) {
      expect(parseExecTimeoutValue(bad), bad).toHaveProperty('error');
    }
  });
});

describe('validateExecTarget', () => {
  it('requires a target and rejects broadcast "all"', () => {
    expect(validateExecTarget('')).toMatch(/required/i);
    expect(validateExecTarget('   ')).toMatch(/required/i);
    expect(validateExecTarget('all')).toMatch(/all/i);
    expect(validateExecTarget('ALL')).toMatch(/all/i);
    expect(validateExecTarget('agent-123')).toBeNull();
  });
});

describe('parseExecPayload', () => {
  it('parses a revealed exec payload', () => {
    const parsed = parseExecPayload(
      JSON.stringify({
        agentId: 'a-1',
        agentName: 'worker',
        command: ['python3', '-c', 'print(1)'],
        timeoutSeconds: 180,
      })
    );
    expect(parsed).toEqual({
      agentId: 'a-1',
      agentName: 'worker',
      command: ['python3', '-c', 'print(1)'],
      timeoutSeconds: 180,
      commandRedacted: false,
      argc: undefined,
      commandSha256: undefined,
    });
  });

  it('parses a redacted exec payload and never exposes command even if malformed JSON carries one', () => {
    const sentinel = 'SUPER-SECRET-TOKEN';
    const parsed = parseExecPayload(
      JSON.stringify({
        agentId: 'a-1',
        agentName: 'worker',
        timeoutSeconds: 60,
        commandRedacted: true,
        argc: 3,
        commandSha256: 'deadbeef1234',
        command: ['echo', sentinel],
      })
    );
    expect(parsed.commandRedacted).toBe(true);
    expect(parsed.command).toEqual([]);
    expect(parsed.argc).toBe(3);
    expect(parsed.commandSha256).toBe('deadbeef1234');
  });

  it('handles malformed JSON gracefully', () => {
    expect(parseExecPayload('not-json')).toEqual({
      agentId: '',
      agentName: '',
      command: [],
      timeoutSeconds: 0,
      commandRedacted: false,
      argc: undefined,
      commandSha256: undefined,
    });
  });
});

describe('formatExecDuration, argvEqual, isAgentSelectableForExec, note', () => {
  it('formats durations cleanly in ms or s', () => {
    expect(formatExecDuration(undefined)).toBe('-');
    expect(formatExecDuration(0)).toBe('0ms');
    expect(formatExecDuration(42)).toBe('42ms');
    expect(formatExecDuration(1000)).toBe('1s');
    expect(formatExecDuration(1500)).toBe('1.5s');
    expect(formatExecDuration(2345)).toBe('2.35s');
  });

  it('compares argv arrays element-by-element', () => {
    expect(argvEqual(['a', 'b'], ['a', 'b'])).toBe(true);
    expect(argvEqual(['a', ''], ['a', ''])).toBe(true);
    expect(argvEqual(['a'], ['a', ''])).toBe(false);
    expect(argvEqual(['a b'], ['a', 'b'])).toBe(false);
  });

  it('checks agent runtime and attach capability when present', () => {
    expect(isAgentSelectableForExec({ id: 'a1', name: 'w1' })).toEqual({ selectable: true });
    expect(
      isAgentSelectableForExec({
        id: 'a2',
        name: 'w2',
        _capabilities: { actions: ['read', 'attach'] },
      })
    ).toEqual({ selectable: true });
    expect(
      isAgentSelectableForExec({
        id: 'a3',
        name: 'w3',
        _capabilities: { actions: ['read', 'update'] },
      })
    ).toEqual({ selectable: false, reason: 'no attach permission' });
    expect(
      isAgentSelectableForExec({
        id: 'a4',
        name: 'w4',
        runtime: 'managed:vertex',
        _capabilities: { actions: ['read', 'attach'] },
      })
    ).toEqual({ selectable: false, reason: 'managed runtime' });
  });

  it('exports the exact required no-output note', () => {
    expect(EXEC_NO_OUTPUT_NOTE).toBe("Command output isn't stored; check the agent's own logs.");
  });
});
