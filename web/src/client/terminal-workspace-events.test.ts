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

import { afterEach, describe, expect, it, vi } from 'vitest';
import { dispatchTeardown } from '../utils/auth.js';
import {
  CURRENT_AGENT_CHANGED_EVENT,
  TERMINAL_SESSION_COUNT_EVENT,
  getAgentPageCurrentAgent,
  getChatCurrentAgent,
  getTerminalWorkspaceSnapshot,
  publishTerminalWorkspaceSnapshot,
  resetTerminalWorkspaceEventsForTests,
  setAgentPageCurrentAgent,
  setChatCurrentAgent,
  type TerminalSessionCountDetail,
} from './terminal-workspace-events.js';

describe('terminal-workspace-events snapshot and current-agent state', () => {
  afterEach(() => {
    resetTerminalWorkspaceEventsForTests();
  });

  it('starts empty and retains the latest published snapshot while keeping detail.count compatible', () => {
    expect(getTerminalWorkspaceSnapshot()).toEqual({
      count: 0,
      sessions: [],
      focusedAgentId: null,
    });

    const received: TerminalSessionCountDetail[] = [];
    const onCount = (e: Event): void => {
      received.push((e as CustomEvent<TerminalSessionCountDetail>).detail);
    };
    window.addEventListener(TERMINAL_SESSION_COUNT_EVENT, onCount);
    try {
      publishTerminalWorkspaceSnapshot({
        count: 2,
        sessions: [
          {
            agentId: 'agent-1',
            name: 'alpha-dev',
            dot: { colour: 'green', meaning: 'Connected' },
          },
          {
            agentId: 'agent-2',
            name: 'beta-dev',
            dot: { colour: 'grey', meaning: 'Not connected' },
          },
        ],
        focusedAgentId: 'agent-2',
      });

      expect(received).toHaveLength(1);
      expect(received[0]?.count).toBe(2);
      expect(received[0]?.focusedAgentId).toBe('agent-2');
      expect(received[0]?.sessions?.map((s) => s.name)).toEqual(['alpha-dev', 'beta-dev']);

      // A late-mounting header reads the retained snapshot synchronously.
      expect(getTerminalWorkspaceSnapshot()).toEqual({
        count: 2,
        sessions: [
          {
            agentId: 'agent-1',
            name: 'alpha-dev',
            dot: { colour: 'green', meaning: 'Connected' },
          },
          {
            agentId: 'agent-2',
            name: 'beta-dev',
            dot: { colour: 'grey', meaning: 'Not connected' },
          },
        ],
        focusedAgentId: 'agent-2',
      });
    } finally {
      window.removeEventListener(TERMINAL_SESSION_COUNT_EVENT, onCount);
    }
  });

  it('tracks chat and agent-page current agents independently and deduplicates unchanged updates', () => {
    const onAgentChange = vi.fn();
    window.addEventListener(CURRENT_AGENT_CHANGED_EVENT, onAgentChange);
    try {
      setChatCurrentAgent({ agentId: 'chat-a', name: 'Chat Agent' });
      expect(getChatCurrentAgent()).toEqual({ agentId: 'chat-a', name: 'Chat Agent' });
      expect(getAgentPageCurrentAgent()).toBeNull();
      expect(onAgentChange).toHaveBeenCalledTimes(1);

      // Identical repeat fires nothing.
      setChatCurrentAgent({ agentId: 'chat-a', name: 'Chat Agent' });
      expect(onAgentChange).toHaveBeenCalledTimes(1);

      setAgentPageCurrentAgent({ agentId: 'detail-b', name: 'Detail Agent' });
      expect(getChatCurrentAgent()).toEqual({ agentId: 'chat-a', name: 'Chat Agent' });
      expect(getAgentPageCurrentAgent()).toEqual({ agentId: 'detail-b', name: 'Detail Agent' });
      expect(onAgentChange).toHaveBeenCalledTimes(2);
    } finally {
      window.removeEventListener(CURRENT_AGENT_CHANGED_EVENT, onAgentChange);
    }
  });

  it('clears the workspace snapshot and current-agent slots on ACCOUNT_TEARDOWN_EVENT', () => {
    publishTerminalWorkspaceSnapshot({
      count: 1,
      sessions: [
        {
          agentId: 'agent-1',
          name: 'alpha-dev',
          dot: { colour: 'green', meaning: 'Connected' },
        },
      ],
      focusedAgentId: 'agent-1',
    });
    setChatCurrentAgent({ agentId: 'chat-a', name: 'Chat Agent' });
    setAgentPageCurrentAgent({ agentId: 'detail-b', name: 'Detail Agent' });

    const countEvents: TerminalSessionCountDetail[] = [];
    const onCount = (e: Event): void => {
      countEvents.push((e as CustomEvent<TerminalSessionCountDetail>).detail);
    };
    const onAgentChange = vi.fn();
    window.addEventListener(TERMINAL_SESSION_COUNT_EVENT, onCount);
    window.addEventListener(CURRENT_AGENT_CHANGED_EVENT, onAgentChange);
    try {
      dispatchTeardown('logout');

      expect(getTerminalWorkspaceSnapshot()).toEqual({
        count: 0,
        sessions: [],
        focusedAgentId: null,
      });
      expect(getChatCurrentAgent()).toBeNull();
      expect(getAgentPageCurrentAgent()).toBeNull();
      expect(countEvents).toEqual([{ count: 0, sessions: [], focusedAgentId: null }]);
      expect(onAgentChange).toHaveBeenCalledTimes(1);
    } finally {
      window.removeEventListener(TERMINAL_SESSION_COUNT_EVENT, onCount);
      window.removeEventListener(CURRENT_AGENT_CHANGED_EVENT, onAgentChange);
    }
  });
});
