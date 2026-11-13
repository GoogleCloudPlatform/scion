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

import * as crypto from 'node:crypto';
import * as http from 'node:http';
import { expect, test } from '@playwright/test';
import {
  canonicalAgentUserDMKey,
  classifyChoiceResponseDelivery,
  createTestAgent,
  deriveAgentSigningKey,
  mintAgentToken,
} from '../harness/agent.js';
import { E2E_SESSION_SECRET } from '../harness/hub.js';

const AGENT_ID = '5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d';
const PROJECT_ID = '11111111-2222-4333-8444-555555555555';

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

  const expectedSig = crypto
    .createHmac('sha256', deriveAgentSigningKey(E2E_SESSION_SECRET))
    .update(`${parts[0]}.${parts[1]}`)
    .digest('base64url');
  expect(parts[2]).toBe(expectedSig);
});

test('classifyChoiceResponseDelivery distinguishes confirm vs override vs unreachable', () => {
  expect(classifyChoiceResponseDelivery('dispatched', '')).toBe('logged_without_delivery');
  expect(
    classifyChoiceResponseDelivery('failed', 'broker delivery failed: agent container not found')
  ).toBe('attempted_delivery');
  expect(classifyChoiceResponseDelivery('failed', 'Agent unreachable (phase: stopped)')).toBe(
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
                type: 'choice-response',
                dispatchState: 'dispatched',
                dispatchFailureReason: '',
              },
              {
                id: 'resp-override',
                type: 'choice-response',
                dispatchState: pollCount >= 2 ? 'failed' : 'dispatched',
                dispatchFailureReason:
                  pollCount >= 2 ? 'broker delivery failed: missing container' : '',
              },
            ],
            messageChoiceResponses: {
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
    const sendRes = await agent.sendChoicePrompt({
      conversationId: 'conv-1',
      choices: [{ question: 'Proceed?', choices: ['Yes', 'No'] }],
    });
    expect(sendRes.message_id).toBe('msg-prompt-1');
    expect(seenCalls[2]?.body).toMatchObject({
      conversation_ref: 'conv:conv-1',
      choices: [{ question: 'Proceed?', choices: ['Yes', 'No'] }],
    });

    // Polling until the override settles also proves the earlier confirm stayed logged_without_delivery.
    const settled = await agent.waitForChoiceResponseDelivery('topic-1', {
      promptMessageId: 'msg-prompt-1',
    });
    expect(settled.messageId).toBe('resp-override');
    expect(settled.attemptedAgentDelivery).toBe(true);

    const all = await agent.inspectChoiceResponses('topic-1');
    const confirmRow = all.find((r) => r.messageId === 'resp-confirm');
    expect(confirmRow?.deliveryClassification).toBe('logged_without_delivery');
    expect(confirmRow?.attemptedAgentDelivery).toBe(false);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});
