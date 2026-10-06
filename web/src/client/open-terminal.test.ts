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
 * Pins `agentGraphHref`'s exact output. The chat toolbar (pages/chat.ts),
 * members sidebar (chat-members.ts) and message context menu
 * (chat-thread.ts) all render this as a real `href` and pass it straight to
 * `navigateTo`, so a wrong query-string separator or an unencoded character
 * would silently break navigation or point at the wrong project/agent.
 * These tests assert literal strings rather than re-deriving the expected
 * value with `encodeURIComponent`, so a regression in the helper can't also
 * "fix" the assertion.
 */

import { describe, it, expect } from 'vitest';
import { agentGraphHref } from './open-terminal.js';

describe('agentGraphHref', () => {
  it('builds the plain graph URL for a project and agent id', () => {
    expect(agentGraphHref('proj-1', 'agent-1')).toBe('/agents/graph?project=proj-1&focus=agent-1');
  });

  it('percent-encodes characters that would otherwise be interpreted as path/query syntax', () => {
    expect(agentGraphHref('proj/1', 'agent 1')).toBe(
      '/agents/graph?project=proj%2F1&focus=agent%201'
    );
  });

  it('percent-encodes characters that would otherwise break the query string', () => {
    expect(agentGraphHref('a&b=c', 'x?y#z')).toBe(
      '/agents/graph?project=a%26b%3Dc&focus=x%3Fy%23z'
    );
  });
});
