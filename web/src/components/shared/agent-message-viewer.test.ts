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
 * The message viewer lists only message records from the message log:
 * lifecycle entries (log_kind=event, or no sender and no recipient) do
 * not render as "unknown -> unknown" rows.
 */

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

beforeAll(async () => {
  await import('./agent-message-viewer.js');
});

afterEach(() => {
  document.body.innerHTML = '';
});

function entry(insertId: string, labels: Record<string, string>) {
  return {
    timestamp: '2026-09-23T15:00:00.000Z',
    severity: 'INFO',
    message: 'log line',
    labels,
    jsonPayload: {},
    insertId,
  };
}

describe('scion-agent-message-viewer message log filtering', () => {
  it('keeps message records and drops lifecycle entries', async () => {
    const el: AnyEl = document.createElement('scion-agent-message-viewer');
    el.projectId = 'p1';
    document.body.appendChild(el);

    el.mergeEntries([
      entry('record', { sender: 'user:alice', recipient: 'agent:bob', msg_type: 'instruction' }),
      // Lifecycle line tagged as an event, even with sender/recipient.
      entry('event', { sender: 'agent:bob', recipient: 'user:alice', log_kind: 'event' }),
      // Older lifecycle line with neither sender nor recipient.
      entry('bare', { msg_type: 'instruction', recipient_id: 'u1' }),
    ]);
    await el.updateComplete;

    const ids = (el.messages as { insertId: string }[]).map((m) => m.insertId);
    expect(ids).toEqual(['record']);

    const actors = Array.from(
      el.shadowRoot.querySelectorAll('.msg-actor, .msg-target') as NodeListOf<HTMLElement>
    ).map((n) => (n.textContent ?? '').trim());
    expect(actors).toEqual(['user:alice', 'agent:bob']);
    expect(actors).not.toContain('unknown');
  });
});
