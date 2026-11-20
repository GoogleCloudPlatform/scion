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
 * Self-tests of the containerless real-hub agent fixture (`../harness/agent.ts`)
 * against an in-process HTTP stand-in (no Go binary, no browser), exercised by
 * `npm run test:e2e-support` in CI (ptone/scion#7776).
 */

import { execFileSync } from 'node:child_process';
import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as http from 'node:http';
import * as os from 'node:os';
import * as path from 'node:path';
import { expect, test } from '@playwright/test';
import {
  canonicalAgentUserDMKey,
  classifyDecisionResponseDelivery,
  createTestAgent,
  deriveAgentSigningKey,
  mintAgentToken,
} from '../harness/agent.js';
import { E2E_SESSION_SECRET } from '../harness/hub.js';

const AGENT_ID = '5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d';
const PROJECT_ID = '11111111-2222-4333-8444-555555555555';

// Independent golden SHA-256 digest of "scion-hub-signing-key:agent_signing_key:e2e-test-secret"
// (matching `deriveSharedSigningKey(secret, "agent_signing_key")` in `pkg/hub/server.go`).
const GOLDEN_E2E_AGENT_SIGNING_KEY_HEX =
  '028d177d8052bc12ca25fd7b2806535e9c337c8649878fbf992883474b6f7e76';

test('deriveAgentSigningKey and mintAgentToken match deterministic golden HS256 vector', () => {
  expect(deriveAgentSigningKey(E2E_SESSION_SECRET).toString('hex')).toBe(
    GOLDEN_E2E_AGENT_SIGNING_KEY_HEX
  );

  const goldenToken = mintAgentToken({
    agentId: AGENT_ID,
    projectId: PROJECT_ID,
    scopes: ['project:read', 'agent:status:update'],
    nowSeconds: 1_700_000_000,
    ttlSeconds: 3600,
    jti: '00000000-0000-4000-8000-000000000001',
  });
  const expectedGoldenJwt =
    'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.' +
    'eyJpc3MiOiJzY2lvbi1odWIiLCJzdWIiOiI1ZjFjMmQzZS02YjFhLTRjNTUtOWYzZS0wZDZlN2ExYjJjM2QiLCJhdWQiOlsic2Npb24taHViLWFwaSJdLCJpYXQiOjE3MDAwMDAwMDAsIm5iZiI6MTcwMDAwMDAwMCwiZXhwIjoxNzAwMDAzNjAwLCJqdGkiOiIwMDAwMDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwMDEiLCJwcm9qZWN0X2lkIjoiMTExMTExMTEtMjIyMi00MzMzLTg0NDQtNTU1NTU1NTU1NTU1Iiwic2NvcGVzIjpbInByb2plY3Q6cmVhZCIsImFnZW50OnN0YXR1czp1cGRhdGUiXSwic2NvcGVfc2NoZW1hIjoxfQ.' +
    'wEx_bOfR2_sMpiFnSoNWxZr1Ap_nWa0-tjME-2YlLdI';
  expect(goldenToken).toBe(expectedGoldenJwt);
});

test('mintAgentToken signs HS256 claims matching pkg/hub/agenttoken.go', () => {
  const token = mintAgentToken({
    agentId: AGENT_ID,
    projectId: PROJECT_ID,
    role: 'baseline',
  });
  const parts = token.split('.');
  expect(parts).toHaveLength(3);

  const header = JSON.parse(Buffer.from(parts[0], 'base64url').toString('utf8')) as {
    alg: string;
    typ: string;
  };
  expect(header).toEqual({ alg: 'HS256', typ: 'JWT' });

  const payload = JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf8')) as {
    iss: string;
    sub: string;
    aud: string[];
    project_id: string;
    scopes: string[];
    scope_schema: number;
    jti: string;
  };
  expect(payload.iss).toBe('scion-hub');
  expect(payload.sub).toBe(AGENT_ID);
  expect(payload.aud).toEqual(['scion-hub-api']);
  expect(payload.project_id).toBe(PROJECT_ID);
  expect(payload.scope_schema).toBe(1);
  expect(payload.scopes).toContain('project:agent:notify');
  expect(payload.scopes).toContain('agent:status:update');

  // Verify against the hard-coded golden key (without calling deriveAgentSigningKey).
  const expectedSig = crypto
    .createHmac('sha256', Buffer.from(GOLDEN_E2E_AGENT_SIGNING_KEY_HEX, 'hex'))
    .update(`${parts[0]}.${parts[1]}`)
    .digest('base64url');
  expect(parts[2]).toBe(expectedSig);
});

test('classifyDecisionResponseDelivery distinguishes confirm vs override vs unreachable', () => {
  expect(classifyDecisionResponseDelivery('dispatched', '')).toBe('logged_without_delivery');
  expect(
    classifyDecisionResponseDelivery('failed', 'broker delivery failed: agent container not found')
  ).toBe('attempted_delivery');
  expect(classifyDecisionResponseDelivery('failed', 'Agent unreachable (phase: stopped)')).toBe(
    'agent_unreachable'
  );
  expect(canonicalAgentUserDMKey('agent-a', 'user-b')).toBe('dm:agent:agent-a:user:user-b');
});

test('createTestAgent provisions with provisionOnly, authenticates, and inspects delivery', async () => {
  const seenCalls: Array<{
    method: string;
    url: string;
    agentToken: string;
    auth: string;
    body: unknown;
  }> = [];
  let pollCount = 0;

  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => {
      const raw = Buffer.concat(chunks).toString('utf8');
      const body: unknown = raw ? JSON.parse(raw) : null;
      seenCalls.push({
        method: req.method ?? '',
        url: req.url ?? '',
        agentToken: String(req.headers['x-scion-agent-token'] ?? ''),
        auth: String(req.headers.authorization ?? ''),
        body,
      });

      if (req.method === 'POST' && req.url === `/api/v1/projects/${PROJECT_ID}/agents`) {
        res.writeHead(201, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            agent: {
              id: AGENT_ID,
              slug: 'test-asker',
              name: 'test-asker',
              projectId: PROJECT_ID,
            },
          })
        );
        return;
      }

      if (req.method === 'POST' && req.url === `/api/v1/agents/${AGENT_ID}/status`) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ applied: true }));
        return;
      }

      if (req.method === 'POST' && req.url === `/api/v1/agents/${AGENT_ID}/outbound-message`) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            message_id: 'msg-prompt-1',
            status: 'sent',
            recipient: 'conv:conv-1',
            recipient_id: 'conv-1',
            conversation_id: 'conv-1',
            decisions: [{ key: 'D1', version: 1, ref: 'D1@v1', row_id: 'row-1', replaced: null }],
          })
        );
        return;
      }

      if (
        req.method === 'GET' &&
        req.url?.startsWith('/api/v1/chat/conversations/topic-1/messages')
      ) {
        pollCount += 1;
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            messages: [
              {
                id: 'resp-confirm',
                type: 'decision-response',
                dispatchState: 'dispatched',
                dispatchFailureReason: '',
              },
              {
                id: 'resp-override',
                type: 'decision-response',
                dispatchState: pollCount >= 2 ? 'failed' : 'dispatched',
                dispatchFailureReason:
                  pollCount >= 2 ? 'broker delivery failed: missing container' : '',
              },
            ],
            messageDecisionResponses: {
              'resp-confirm': {
                promptMessageId: 'msg-confirm-prompt',
                rowId: 'row-c',
                key: 'DC',
                choiceId: 'yes',
                label: 'Yes',
              },
              'resp-override': {
                promptMessageId: 'msg-prompt-1',
                rowId: 'row-1',
                key: 'D1',
                choiceId: 'no',
                label: 'No',
              },
            },
          })
        );
        return;
      }

      res.writeHead(404);
      res.end();
    });
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', () => resolve()));
  const addr = server.address();
  if (!addr || typeof addr !== 'object') {
    server.close();
    throw new Error('Could not bind local server');
  }
  const baseURL = `http://127.0.0.1:${addr.port}`;

  try {
    const agent = await createTestAgent(
      { baseURL, devToken: 'scion_dev_fake' },
      { projectId: PROJECT_ID, name: 'test-asker', messageMode: 'project' }
    );
    expect(agent.id).toBe(AGENT_ID);
    expect(seenCalls[0]?.body).toEqual({
      name: 'test-asker',
      provisionOnly: true,
      agentRole: 'baseline',
      messageMode: 'project',
    });
    expect(seenCalls[1]?.url).toBe(`/api/v1/agents/${AGENT_ID}/status`);
    expect(seenCalls[1]?.agentToken).toBe(agent.token);

    // Addressing with conversationId alone normalizes to conversation_ref: conv:<id>.
    const sendRes = await agent.sendDecisionPrompt({
      conversationId: 'conv-1',
      decisions: [{ question: 'Proceed?', choices: ['Yes', 'No'] }],
    });
    expect(sendRes.message_id).toBe('msg-prompt-1');
    expect(seenCalls[2]?.body).toMatchObject({
      conversation_ref: 'conv:conv-1',
      decisions: [{ question: 'Proceed?', choices: ['Yes', 'No'] }],
    });

    // In practice on a containerless agent, polling until the override settles
    // lets callers re-inspect and verify the earlier confirm stayed logged_without_delivery.
    const settled = await agent.waitForDecisionResponseDelivery('topic-1', {
      promptMessageId: 'msg-prompt-1',
    });
    expect(settled.messageId).toBe('resp-override');
    expect(settled.attemptedAgentDelivery).toBe(true);

    const all = await agent.inspectDecisionResponses('topic-1');
    const confirmRow = all.find((r) => r.messageId === 'resp-confirm');
    expect(confirmRow?.deliveryClassification).toBe('logged_without_delivery');
    expect(confirmRow?.attemptedAgentDelivery).toBe(false);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});

test('createTestAgent records matching agent_credentials row when hub.dbDir is provided', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-agent-fixture-'));
  const dbPath = path.join(tmpDir, 'e2e.db');
  // Hand-written DDL mirrors pkg/ent/schema/agentcredential.go and must be kept in sync.
  execFileSync(
    'python3',
    [
      '-c',
      `
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.execute("""
CREATE TABLE agent_credentials (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    token_jti_hash TEXT NOT NULL UNIQUE,
    run_id TEXT NOT NULL DEFAULT '',
    issued_at DATETIME NOT NULL,
    expires_at DATETIME NOT NULL,
    revoked_at DATETIME,
    revoked_by TEXT,
    revoke_reason TEXT,
    last_seen_at DATETIME
)
""")
con.commit()
con.close()
`,
      dbPath,
    ],
    { encoding: 'utf8' }
  );

  const server = http.createServer((req, res) => {
    req.resume();
    req.on('end', () => {
      if (req.method === 'POST' && req.url === `/api/v1/projects/${PROJECT_ID}/agents`) {
        res.writeHead(201, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            agent: {
              id: AGENT_ID,
              slug: 'db-asker',
              name: 'db-asker',
              projectId: PROJECT_ID,
            },
          })
        );
        return;
      }
      if (req.method === 'POST' && req.url === `/api/v1/agents/${AGENT_ID}/status`) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ applied: true }));
        return;
      }
      res.writeHead(404);
      res.end();
    });
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', () => resolve()));
  const addr = server.address();
  if (!addr || typeof addr !== 'object') {
    server.close();
    throw new Error('Could not bind local server');
  }

  try {
    const agent = await createTestAgent(
      { baseURL: `http://127.0.0.1:${addr.port}`, devToken: 'scion_dev_fake', dbDir: tmpDir },
      { projectId: PROJECT_ID, name: 'db-asker' }
    );
    const payload = JSON.parse(
      Buffer.from(agent.token.split('.')[1], 'base64url').toString('utf8')
    ) as { jti: string };
    const expectedHash = crypto.createHash('sha256').update(payload.jti).digest('hex');

    const rowJson = execFileSync(
      'python3',
      [
        '-c',
        `
import json, sqlite3, sys
con = sqlite3.connect(sys.argv[1])
row = con.execute(
    "SELECT agent_id, project_id, token_jti_hash, run_id, revoked_at FROM agent_credentials"
).fetchone()
con.close()
print(json.dumps(list(row) if row else None))
`,
        dbPath,
      ],
      { encoding: 'utf8' }
    );
    expect(JSON.parse(rowJson.trim())).toEqual([AGENT_ID, PROJECT_ID, expectedHash, '', null]);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('createTestAgent throws explicit missing-id error on empty 2xx response body', async () => {
  const server = http.createServer((_req, res) => {
    res.writeHead(201, { 'Content-Type': 'application/json' });
    res.end('');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', () => resolve()));
  const addr = server.address();
  if (!addr || typeof addr !== 'object') {
    server.close();
    throw new Error('Could not bind local server');
  }
  try {
    await expect(
      createTestAgent(
        { baseURL: `http://127.0.0.1:${addr.port}`, devToken: 'scion_dev_fake' },
        { projectId: PROJECT_ID }
      )
    ).rejects.toThrow(/response missing agent id\/slug/);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});
