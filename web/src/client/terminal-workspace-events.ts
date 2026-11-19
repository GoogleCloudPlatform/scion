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

import { ACCOUNT_TEARDOWN_EVENT } from '../utils/auth.js';

/** The colour of a terminal rail / dropdown row's status dot. */
export type TerminalSessionDotColour = 'green' | 'amber' | 'red' | 'grey' | 'neutral';

/** A status dot's colour and its short meaning, shown as tooltip and spoken text. */
export interface TerminalSessionDot {
  readonly colour: TerminalSessionDotColour;
  readonly meaning: string;
}

/** Summary of one open terminal session in the rail's current sort order. */
export interface TerminalSessionSummary {
  readonly agentId: string;
  readonly name: string;
  readonly dot: TerminalSessionDot;
}

/** Latest retained snapshot of the terminal workspace rail for header consumers. */
export interface TerminalWorkspaceSnapshot {
  readonly count: number;
  readonly sessions: readonly TerminalSessionSummary[];
  readonly focusedAgentId: string | null;
}

export interface TerminalSessionCountDetail {
  readonly count: number;
  readonly sessions?: readonly TerminalSessionSummary[];
  readonly focusedAgentId?: string | null;
}

export const TERMINAL_SESSION_COUNT_EVENT = 'scion:terminal-session-count';

/** Custom MIME type for terminal drag payloads. */
export const TERMINAL_DRAG_MIME = 'application/x-scion-terminal';

export interface TerminalPaletteNewAgentDetail {
  readonly agentId: string;
}

export const TERMINAL_PALETTE_NEW_AGENT_EVENT = 'scion:terminal-palette-new-agent';
export const TERMINAL_PANE_DIALOG_OPEN_EVENT = 'scion:terminal-pane-dialog-open';
export const TERMINAL_DROPDOWN_SELECT_EVENT = 'scion:terminal-dropdown-select';

export interface TerminalDropdownSelectDetail {
  readonly agentId: string;
}

export const CURRENT_AGENT_CHANGED_EVENT = 'scion:current-agent-changed';

export interface CurrentAgentInfo {
  readonly agentId: string;
  readonly name: string;
}

const EMPTY_SNAPSHOT: TerminalWorkspaceSnapshot = {
  count: 0,
  sessions: [],
  focusedAgentId: null,
};

let latestSnapshot: TerminalWorkspaceSnapshot = EMPTY_SNAPSHOT;
let chatAgent: CurrentAgentInfo | null = null;
let pageAgent: CurrentAgentInfo | null = null;

function setSlot(
  prev: CurrentAgentInfo | null,
  next: CurrentAgentInfo | null,
  assign: (v: CurrentAgentInfo | null) => void
): void {
  const val = next ? { agentId: next.agentId, name: next.name } : null;
  if (prev?.agentId === val?.agentId && prev?.name === val?.name && Boolean(prev) === Boolean(val))
    return;
  assign(val);
  if (typeof window !== 'undefined') window.dispatchEvent(new Event(CURRENT_AGENT_CHANGED_EVENT));
}

export function getTerminalWorkspaceSnapshot(): TerminalWorkspaceSnapshot {
  return latestSnapshot;
}

export function publishTerminalWorkspaceSnapshot(snapshot: TerminalWorkspaceSnapshot): void {
  latestSnapshot = {
    count: Math.max(0, snapshot.count),
    sessions: [...snapshot.sessions],
    focusedAgentId: snapshot.focusedAgentId ?? null,
  };
  if (typeof window !== 'undefined') {
    window.dispatchEvent(
      new CustomEvent<TerminalSessionCountDetail>(TERMINAL_SESSION_COUNT_EVENT, {
        detail: latestSnapshot,
      })
    );
  }
}

export function getChatCurrentAgent(): CurrentAgentInfo | null {
  return chatAgent;
}

export function setChatCurrentAgent(agent: CurrentAgentInfo | null): void {
  setSlot(chatAgent, agent, (v) => {
    chatAgent = v;
  });
}

export function getAgentPageCurrentAgent(): CurrentAgentInfo | null {
  return pageAgent;
}

export function setAgentPageCurrentAgent(agent: CurrentAgentInfo | null): void {
  setSlot(pageAgent, agent, (v) => {
    pageAgent = v;
  });
}

if (typeof window !== 'undefined') {
  window.addEventListener(ACCOUNT_TEARDOWN_EVENT, () => {
    const hadSnap =
      latestSnapshot.count !== 0 ||
      latestSnapshot.sessions.length !== 0 ||
      latestSnapshot.focusedAgentId !== null;
    const hadAgent = chatAgent !== null || pageAgent !== null;
    latestSnapshot = EMPTY_SNAPSHOT;
    chatAgent = null;
    pageAgent = null;
    if (hadSnap) {
      window.dispatchEvent(
        new CustomEvent<TerminalSessionCountDetail>(TERMINAL_SESSION_COUNT_EVENT, {
          detail: EMPTY_SNAPSHOT,
        })
      );
    }
    if (hadAgent) window.dispatchEvent(new Event(CURRENT_AGENT_CHANGED_EVENT));
  });
}

export function resetTerminalWorkspaceEventsForTests(): void {
  latestSnapshot = EMPTY_SNAPSHOT;
  chatAgent = null;
  pageAgent = null;
}
