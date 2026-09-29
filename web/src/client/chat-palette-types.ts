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
 * chat quick command palette (Phase 1: the Agents/DM slice only).
 *
 * Type-only module: importing this file must not eagerly pull in the
 * `<scion-chat-switcher>` component or any API client. Later phases add
 * 'threads' | 'people' | 'documents' to {@link PaletteGroup} and extend
 * {@link PaletteTarget}; the shapes below are written so those additions are
 * additive, not breaking.
 */

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
 * The navigable result of a palette selection. Phase 1 only produces `dm`
 * targets (Agents group); `thread` and `document` targets are added by
 * phases 2 and 4 respectively.
 */
export type PaletteTarget = PaletteDmTarget;

/**
 * The four groups the full palette renders (Agents, Threads, People,
 * Documents, in that reading order). Phase 1 only ever populates 'agents'.
 */
export type PaletteGroup = 'agents' | 'threads' | 'people' | 'documents';

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
