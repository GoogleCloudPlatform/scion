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

import { describe, it, expect } from 'vitest';
import {
  emptyAgentSecretRow,
  MAX_CREATE_AGENT_SECRETS,
  toCreateAgentSecrets,
  validateAgentSecretRows,
  type AgentSecretDraftRow,
} from './agent-create-secrets.js';

function row(overrides: Partial<AgentSecretDraftRow> = {}): AgentSecretDraftRow {
  return { ...emptyAgentSecretRow(), ...overrides };
}

describe('agent-create-secrets validation and serialization', () => {
  it('ignores completely blank rows and returns empty payload', () => {
    const rows = [emptyAgentSecretRow(), emptyAgentSecretRow()];
    expect(validateAgentSecretRows(rows)).toEqual([]);
    expect(toCreateAgentSecrets(rows)).toEqual([]);
  });

  it('serializes valid environment, file, and variable secrets', () => {
    const rows = [
      row({ key: 'GITHUB_TOKEN', value: 'ghp_123', type: 'environment', injectionMode: 'always' }),
      row({
        key: 'DEPLOY_KEY',
        value: '-----BEGIN KEY-----\nabc\n-----END KEY-----',
        type: 'file',
        target: '~/.ssh/id_ed25519',
        injectionMode: 'as_needed',
      }),
      row({ key: 'API_CFG', value: '{"a":1}', type: 'variable', target: 'api_cfg' }),
    ];
    expect(validateAgentSecretRows(rows)).toEqual([]);
    expect(toCreateAgentSecrets(rows)).toEqual([
      {
        key: 'GITHUB_TOKEN',
        value: 'ghp_123',
        type: 'environment',
        injectionMode: 'always',
      },
      {
        key: 'DEPLOY_KEY',
        value: '-----BEGIN KEY-----\nabc\n-----END KEY-----',
        type: 'file',
        target: '~/.ssh/id_ed25519',
        injectionMode: 'as_needed',
      },
      {
        key: 'API_CFG',
        value: '{"a":1}',
        type: 'variable',
        target: 'api_cfg',
        injectionMode: 'as_needed',
      },
    ]);
  });

  it('rejects reserved SCION_* and GCE_METADATA_* env keys and targets', () => {
    expect(validateAgentSecretRows([row({ key: 'SCION_HUB_TOKEN', value: 'x' })])).toEqual([
      expect.stringContaining('reserved'),
    ]);
    expect(validateAgentSecretRows([row({ key: 'GCE_METADATA_HOST', value: 'x' })])).toEqual([
      expect.stringContaining('reserved'),
    ]);
    expect(
      validateAgentSecretRows([row({ key: 'MY_VAR', target: 'SCION_INTERNAL', value: 'x' })])
    ).toEqual([expect.stringContaining('reserved')]);
  });

  it('rejects systemone.instance. and webhook_endpoint. reserved key prefixes', () => {
    expect(validateAgentSecretRows([row({ key: 'systemone.instance.foo', value: 'x' })])).toEqual([
      expect.stringContaining('systemone.instance.'),
    ]);
    expect(validateAgentSecretRows([row({ key: 'webhook_endpoint.hook1', value: 'x' })])).toEqual([
      expect.stringContaining('webhook_endpoint.'),
    ]);
  });

  it('rejects invalid key characters and missing keys/values', () => {
    expect(validateAgentSecretRows([row({ key: 'BAD KEY', value: 'x' })])).toEqual([
      expect.stringContaining("whitespace or '='"),
    ]);
    expect(validateAgentSecretRows([row({ key: 'A=B', value: 'x' })])).toEqual([
      expect.stringContaining("whitespace or '='"),
    ]);
    expect(validateAgentSecretRows([row({ key: '', value: 'x' })])).toEqual([
      expect.stringContaining('key is required'),
    ]);
    expect(validateAgentSecretRows([row({ key: 'NO_VAL', value: '' })])).toEqual([
      expect.stringContaining('value is required'),
    ]);
  });

  it('rejects duplicate keys', () => {
    const errors = validateAgentSecretRows([
      row({ key: 'DUP_KEY', value: 'one' }),
      row({ key: 'DUP_KEY', value: 'two' }),
    ]);
    expect(errors).toEqual([expect.stringContaining('Duplicate secret key: DUP_KEY')]);
  });

  it('enforces file secret target rules', () => {
    expect(
      validateAgentSecretRows([row({ key: 'SSH_KEY', value: 'pem', type: 'file', target: '' })])
    ).toEqual([expect.stringContaining('requires a target path')]);
    expect(
      validateAgentSecretRows([
        row({ key: 'SSH_KEY', value: 'pem', type: 'file', target: 'relative/path' }),
      ])
    ).toEqual([expect.stringContaining('absolute path')]);
    expect(
      validateAgentSecretRows([
        row({ key: 'SSH_KEY', value: 'pem', type: 'file', target: '/etc/../passwd' }),
      ])
    ).toEqual([expect.stringContaining("must not contain '..'")]);
    expect(
      validateAgentSecretRows([
        row({ key: 'SSH_KEY', value: 'pem', type: 'file', target: '/etc/app/key.pem' }),
      ])
    ).toEqual([]);
  });

  it('rejects more than 50 secrets', () => {
    const rows = Array.from({ length: MAX_CREATE_AGENT_SECRETS + 1 }, (_, i) =>
      row({ key: `KEY_${i}`, value: `val_${i}` })
    );
    const errors = validateAgentSecretRows(rows);
    expect(errors).toContainEqual(expect.stringContaining('At most 50 secrets'));
  });
});
