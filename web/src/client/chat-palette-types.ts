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
 * Shared discriminated target/candidate/group-state types for the native
 * chat quick command palette, covering the Agents/DM, Threads, People and
 * Documents groups.
 *
 * Type-only module: importing this file must not eagerly pull in the
 * `<scion-quick-palette>` component or any API client. The `RecentFile` import
 * below is `import type`-only for the same reason: it must not eagerly pull
 * in the `chatRecentFiles` singleton module.
 */

import type { RecentFile } from './chat-recent-files.js';

/** The kind of DM peer: an agent or a human user. */
export type PeerKind = 'agent' | 'user';

/** Selecting an Agents or People row opens (or creates) that peer's DM. */
export interface PaletteDmTarget {
  kind: 'dm';
  peerKind: PeerKind;
  peerId: string;
  displayName: string;
}

/**
 * Selecting a Threads row switches conversation context in-page via the
 * existing `handleThreadSelect` path. `projectSlug` is the space's *known*
 * slug at candidate-build time — absent (never an empty string,
 * exactOptionalPropertyTypes) when the space has none, in which case the
 * navigation handler must route by `projectId` rather than guess another
 * project's slug.
 */
export interface PaletteThreadTarget {
  kind: 'thread';
  projectId: string;
  threadId: string;
  projectSlug?: string;
  threadName: string;
  defaultAgent?: string;
}

/**
 * Selecting a Documents row opens the page-level file preview for that
 * recent file (an attachment or a resolved container path) without changing
 * conversation context.
 */
export interface PaletteDocumentTarget {
  kind: 'document';
  file: RecentFile;
}

/**
 * Selecting an agent row on a surface that acts on the agent itself rather
 * than on a conversation with it (the terminal view's "Jump to agent"
 * palette attaches to the agent's terminal).
 */
export interface PaletteAgentTarget {
  kind: 'agent';
  agentId: string;
  displayName: string;
}

/**
 * The navigable result of a palette selection: `dm` targets (chat's
 * Agents/People groups), `thread` targets (Threads group), `document`
 * targets (Documents group), or `agent` targets (agent rows on a non-chat
 * surface).
 */
export type PaletteTarget =
  | PaletteDmTarget
  | PaletteThreadTarget
  | PaletteDocumentTarget
  | PaletteAgentTarget;

/**
 * The four groups the full palette renders (Agents, Threads, People,
 * Documents, in that reading order). This exact array is the single source
 * of truth for that reading/Tab order — the ranking comparator
 * (`chat-palette-match.ts`) and the palette's own Tab/Shift+Tab cycling
 * (`quick-palette.ts`) both derive their group ordering from it so the two
 * can never independently drift apart.
 */
export type PaletteGroup = 'agents' | 'threads' | 'people' | 'documents';

/** Reading/Tab order for the four palette groups: Agents, Threads, People, Documents. */
export const PALETTE_GROUP_ORDER: readonly PaletteGroup[] = [
  'agents',
  'threads',
  'people',
  'documents',
];

/** One row in the palette result list. */
export interface PaletteCandidate {
  /** Stable ID: a JSON-encoded tuple, e.g. `["dm","agent","<agentId>"]`. Never a raw label. */
  id: string;
  group: PaletteGroup;
  /** Primary display label (also the highlight target). */
  label: string;
  /** Fields searched for a match: display name, slug, email, etc. */
  searchFields: string[];
  /** Secondary line (slug, project, path) shown under the label. */
  secondaryLabel: string;
  /** Recency signal in epoch ms; 0 for "never" or an invalid/Go-zero timestamp. */
  activityMs: number;
  target: PaletteTarget;
}

/** Per-group load state, keyed by {@link PaletteGroup}. */
export interface GroupState {
  status: 'loading' | 'ready' | 'error';
  candidates: PaletteCandidate[];
  error?: string;
  /**
   * True when `candidates` is a coherent but incomplete snapshot: some of the
   * group's underlying list requests failed while others succeeded (Threads:
   * one or more spaces' thread lists). The group stays `'ready'` and its
   * successful rows remain selectable, with this flag driving a visible
   * incomplete-results notice, rather than flipping to `'error'`, which
   * would hide rows that loaded fine.
   */
  incomplete?: boolean;
}

/** Detail for the `palette-select` event. */
export interface PaletteSelectDetail {
  target: PaletteTarget;
}

/** Reason a `palette-dismiss` event fired. */
export type PaletteDismissReason = 'escape' | 'toggle' | 'backdrop' | 'close';

/** Detail for the `palette-dismiss` event. */
export interface PaletteDismissDetail {
  reason: PaletteDismissReason;
}

/** Detail for the `palette-retry` event: the caller should reload just this group. */
export interface PaletteRetryDetail {
  group: PaletteGroup;
}

/**
 * Build the stable candidate ID for a DM target: a JSON-encoded tuple, so
 * candidate IDs stay stable across renders. Using JSON avoids delimiter
 * collisions with IDs that might themselves contain the tuple separator.
 */
export function dmCandidateId(peerKind: PeerKind, peerId: string): string {
  return JSON.stringify(['dm', peerKind, peerId]);
}

/**
 * Build the stable candidate ID for an Agent target: a JSON-encoded tuple,
 * matching {@link dmCandidateId}'s shape and stability guarantee, and never
 * equal to a DM candidate ID for the same agent.
 */
export function agentCandidateId(agentId: string): string {
  return JSON.stringify(['agent', agentId]);
}

/**
 * Build the stable candidate ID for a Thread target: a JSON-encoded tuple,
 * matching {@link dmCandidateId}'s shape and stability guarantee.
 * `projectId` is included (not just `threadId`) so an ID never collides
 * across projects even though thread IDs are already globally unique in
 * practice.
 */
export function threadCandidateId(projectId: string, threadId: string): string {
  return JSON.stringify(['thread', projectId, threadId]);
}

/**
 * Build the stable candidate ID for a Document target: a JSON-encoded tuple,
 * matching {@link dmCandidateId}/{@link threadCandidateId}'s shape and
 * stability guarantee. `fileKey` is the recent-file's own identity tuple
 * (see `chat-recent-files.ts`), already stable across renders on its own.
 */
export function documentCandidateId(fileKey: string): string {
  return JSON.stringify(['document', fileKey]);
}
