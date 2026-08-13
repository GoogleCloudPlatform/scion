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
 * Injected Skills Panel Component
 *
 * Shared component for managing injected skills across project, user, and hub
 * scopes. Accepts a scope and scopeId prop and calls the appropriate API
 * endpoints. Hub system entries render as read-only with a lock badge.
 *
 * Scopes:
 *   project → GET/POST/PUT/DELETE /api/v1/projects/{scopeId}/injected-skills[/{id}]
 *   user    → GET/POST/PUT/DELETE /api/v1/users/me/injected-skills[/{id}]
 *   hub     → GET/PUT /api/v1/hub/settings/injected-skills
 *             (system entries are always read-only; user_defined entries editable)
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { Skill } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';
import { showToast } from '../../utils/toast.js';
import { showConfirm } from './confirm-dialog.js';

export type InjectedSkillsScope = 'project' | 'user' | 'hub';

// ── Skill URI client-side normalization ──────────────────────────────────────
// Mirrors NormalizeSkillURI in pkg/api/skill_uri_normalize.go.
// The hub's Go implementation is authoritative; these functions provide
// immediate client-side feedback for the most common cases.

/** Result of client-side URI normalization. */
interface NormalizeResult {
  canonical: string;
  /** True when canonical differs from the original input. */
  transformed: boolean;
}

/**
 * Normalizes a user-supplied skill URI to its canonical stored form.
 * Throws an Error with an actionable message for unsupported inputs.
 */
function normalizeSkillURIClient(input: string): NormalizeResult {
  const trimmed = input.trim();
  if (!trimmed) throw new Error('Skill URI is required');
  const lower = trimmed.toLowerCase();
  if (lower.startsWith('gh://')) {
    return { canonical: validateGHShorthand(trimmed), transformed: false };
  }
  if (lower.startsWith('https://github.com/') || lower.startsWith('http://github.com/')) {
    const canonical = normalizeGitHubURL(trimmed);
    return { canonical, transformed: canonical !== trimmed };
  }
  if (lower.startsWith('scion://')) {
    throw new Error('scion:// is not a supported scheme; use skill:// for hub-bank skills');
  }
  // skill://, bare names, other schemes — pass through to the hub for validation.
  return { canonical: trimmed, transformed: false };
}

function validateGHShorthand(uri: string): string {
  let main = uri.slice('gh://'.length);
  let tokenSuffix = '';
  const qIdx = main.indexOf('?');
  if (qIdx >= 0) {
    const query = main.slice(qIdx + 1);
    main = main.slice(0, qIdx);
    if (!query.startsWith('token='))
      throw new Error('Invalid gh:// URI: only ?token=SECRET_NAME is supported');
    const tokenName = query.slice('token='.length);
    if (!tokenName || !/^[A-Z][A-Z0-9_]*$/.test(tokenName)) {
      throw new Error(
        'Invalid gh:// URI: ?token= value must be an uppercase env-var name (e.g. SKILLS_TOKEN)'
      );
    }
    tokenSuffix = '?' + query;
  }
  let refSuffix = '';
  const atIdx = main.lastIndexOf('@');
  if (atIdx >= 0) {
    const ref = main.slice(atIdx + 1);
    if (!ref || ref === '.' || ref === '..')
      throw new Error('Invalid gh:// URI: invalid ref (must not be empty, ".", or "..")');
    main = main.slice(0, atIdx);
    refSuffix = '@' + ref;
  }
  const parts = main.split('/');
  if (parts.length !== 3 || parts.some((p) => !p || p === '.' || p === '..')) {
    throw new Error(
      'Invalid gh:// URI: expected gh://owner/repo/skill-name[@ref][?token=SECRET_NAME]'
    );
  }
  return 'gh://' + main + refSuffix + tokenSuffix;
}

function normalizeGitHubURL(uri: string): string {
  let rest = uri;
  for (const prefix of ['https://github.com/', 'http://github.com/']) {
    if (rest.toLowerCase().startsWith(prefix)) {
      rest = rest.slice(prefix.length);
      break;
    }
  }
  let tokenSuffix = '';
  const qIdx = rest.indexOf('?');
  if (qIdx >= 0) {
    const query = rest.slice(qIdx + 1);
    rest = rest.slice(0, qIdx);
    if (!query.startsWith('token='))
      throw new Error('Invalid GitHub URL: only ?token=SECRET_NAME is supported');
    const tokenName = query.slice('token='.length);
    if (!tokenName || !/^[A-Z][A-Z0-9_]*$/.test(tokenName)) {
      throw new Error(
        'Invalid GitHub URL: ?token= value must be an uppercase env-var name (e.g. SKILLS_TOKEN)'
      );
    }
    tokenSuffix = '?' + query;
  }
  const segments = rest.split('/');
  const [owner, repo, keyword, ref, ...pathParts] = segments;
  if (
    !owner ||
    !repo ||
    !keyword ||
    !ref ||
    owner === '.' ||
    owner === '..' ||
    repo === '.' ||
    repo === '..' ||
    ref === '.' ||
    ref === '..'
  ) {
    throw new Error(
      'Invalid GitHub URL: expected https://github.com/owner/repo/tree/ref/path/to/skill'
    );
  }
  const kw = keyword.toLowerCase();
  let fullPath: string;
  if (kw === 'tree') {
    fullPath = pathParts.join('/');
    if (!fullPath)
      throw new Error(
        'Invalid GitHub URL: missing skill path after ref; example: .../tree/main/skills/my-skill'
      );
  } else if (kw === 'blob') {
    const filePath = pathParts.join('/');
    if (!filePath) throw new Error('Invalid GitHub URL: missing file path after ref');
    const lastSlash = filePath.lastIndexOf('/');
    if (lastSlash < 0)
      throw new Error(
        'Invalid GitHub URL: cannot determine skill directory from blob URL (no parent directory)'
      );
    fullPath = filePath.slice(0, lastSlash);
    if (!fullPath)
      throw new Error('Invalid GitHub URL: cannot determine skill directory from blob URL');
  } else {
    throw new Error(
      `Invalid GitHub URL: expected /tree/ or /blob/ after owner/repo, got /${keyword}/`
    );
  }
  // Use gh:// shorthand for skills/skill-name paths (standard layout).
  const pathSegs = fullPath.split('/');
  if (pathSegs.length === 2 && pathSegs[0].toLowerCase() === 'skills' && pathSegs[1]) {
    return `gh://${owner}/${repo}/${pathSegs[1]}@${ref}${tokenSuffix}`;
  }
  return `https://github.com/${owner}/${repo}/tree/${ref}/${fullPath}${tokenSuffix}`;
}

/** Normalized internal row — covers both SkillInjectionEntry and SkillReference shapes. */
interface SkillRow {
  /** Entry ID — present for project/user scopes (from SkillInjectionEntry); empty for hub. */
  id: string;
  /** Canonical skill URI (field name differs: skillUri vs uri). */
  uri: string;
  /** Alias override, if any. */
  as: string;
  /** Whether a resolution failure is non-fatal. */
  optional: boolean;
  /** Position for drag-based reorder (project/user only). */
  sortOrder: number;
  /** Enriched skill name (if URI resolves to skill bank). */
  skillName: string;
  /** Enriched skill slug (if URI resolves to skill bank). */
  skillSlug: string;
  /** True for hub system entries — cannot be edited or removed. */
  readonly: boolean;
}

@customElement('scion-injected-skills-panel')
export class ScionInjectedSkillsPanel extends LitElement {
  /** Which scope this panel manages. */
  @property() scope: InjectedSkillsScope = 'project';

  /** Project or user UUID; empty for hub scope. */
  @property() scopeId = '';

  /** When true, the entire panel is read-only (used for system hub entries). */
  @property({ type: Boolean }) readonly = false;

  @state() private loading = true;
  @state() private rows: SkillRow[] = [];
  @state() private error: string | null = null;

  // Add dialog
  @state() private dialogOpen = false;
  @state() private dialogMode: 'search' | 'uri' = 'search';
  @state() private dialogSkillQuery = '';
  @state() private dialogSkillResults: Skill[] = [];
  @state() private dialogSkillSearching = false;
  @state() private dialogSelectedSkill: Skill | null = null;
  @state() private dialogUri = '';
  @state() private dialogAs = '';
  @state() private dialogOptional = false;
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;
  /** When a URI input is transformed to canonical form, holds the preview string. */
  @state() private dialogTransformed: string | null = null;

  // Directory discovery (batch-add). Populated by handleDiscoverDirectory() when
  // the user probes a GitHub directory URL for the skills it contains.
  @state() private discoveryDialogOpen = false;
  @state() private discoveredSkills: Array<{ uri: string; name: string }> = [];
  /**
   * Child directory names the backend passed over (no SKILL.md, or a name that
   * could not be turned into a safe skill URI). Surfaced in the selection dialog
   * so a folder the user expected to see gets an explanation rather than
   * vanishing silently.
   */
  @state() private skippedSkillNames: string[] = [];
  @state() private selectedSkillURIs: Set<string> = new Set();
  @state() private discoveryLoading = false;
  @state() private discoveryError: string | null = null;

  // Delete — tracked by index to avoid key collisions on hub rows (all have id='')
  @state() private _deletingIndex: number | null = null;

  // Drag
  @state() private dragSourceIndex: number | null = null;
  @state() private dragOverIndex: number | null = null;

  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private _searchAbortController: AbortController | null = null;
  private _loadAbortController: AbortController | null = null;

  static override styles = [
    resourceStyles,
    css`
      .panel-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: 1rem;
        gap: 1rem;
      }

      .panel-header-info p {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
        margin: 0;
      }

      .drag-handle {
        cursor: grab;
        color: var(--scion-text-muted, #64748b);
        font-size: 1rem;
        display: flex;
        align-items: center;
        padding: 0 0.25rem;
      }

      .drag-handle:active {
        cursor: grabbing;
      }

      tr.drag-over td {
        background: var(--sl-color-primary-50, #eff6ff);
      }

      tr.dragging {
        opacity: 0.4;
      }

      .system-badge {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
        border: 1px solid var(--scion-border, #e2e8f0);
      }

      .system-badge sl-icon {
        font-size: 0.625rem;
      }

      .skill-name {
        font-weight: 600;
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
      }

      .skill-uri {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
        display: inline-flex;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        max-width: 300px;
      }

      .skill-uri-prefix {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        flex-shrink: 1;
        min-width: 0;
      }

      .skill-uri-name {
        white-space: nowrap;
        flex-shrink: 0;
      }

      .skill-info {
        display: flex;
        flex-direction: column;
      }

      .mode-toggle {
        display: flex;
        gap: 0.5rem;
        margin-bottom: 0.75rem;
      }

      .mode-toggle sl-button[variant='primary'] {
        font-weight: 600;
      }

      .search-results {
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        max-height: 200px;
        overflow-y: auto;
        margin-top: 0.5rem;
      }

      .search-result-item {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        cursor: pointer;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        font-size: 0.875rem;
      }

      .search-result-item:last-child {
        border-bottom: none;
      }

      .search-result-item:hover,
      .search-result-item.selected {
        background: var(--sl-color-primary-50, #eff6ff);
      }

      .search-result-item .skill-slug {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      /* Directory-discovery selection dialog. Mirrors the equivalent block in
         resource-import.ts so both checkbox pickers look the same. */
      .selection-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding-bottom: 0.75rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        margin-bottom: 0.75rem;
      }

      .selection-count {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .selection-list {
        display: flex;
        flex-direction: column;
        gap: 0.5rem;
        max-height: 400px;
        overflow-y: auto;
      }

      .selection-item {
        padding: 0.25rem 0;
      }

      .discovery-skipped-note {
        margin: 0.75rem 0 0;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .no-results {
        padding: 0.75rem;
        text-align: center;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    // Lit applies element properties/attributes AFTER connectedCallback via its
    // update cycle. For project-scoped panels, scopeId may still be '' here,
    // which would produce a malformed URL (/api/v1/projects//injected-skills).
    // Guard: only load now if scope is set and (for project) scopeId is non-empty.
    // updated() re-triggers load() once both values are available.
    if (this.scope && (this.scope !== 'project' || this.scopeId)) {
      void this.load();
    }
  }

  override updated(changedProperties: PropertyValues): void {
    if (changedProperties.has('scopeId') || changedProperties.has('scope')) {
      if (this.scope && (this.scope !== 'project' || this.scopeId)) {
        void this.load();
      } else {
        // Clear stale rows when scope/scopeId becomes invalid so that a panel
        // reused with a new (empty) scopeId does not show data from the
        // previous scope. Abort any in-flight load first so it cannot overwrite
        // the cleared state with stale data from the previous scope.
        this._loadAbortController?.abort();
        this._loadAbortController = null;
        this.loading = false;
        this.rows = [];
        this.error = null;
      }
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchTimer) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
    this.cancelSearch();
    this._loadAbortController?.abort();
    this._loadAbortController = null;
  }

  /** Abort any in-flight skill search and clear the controller reference. */
  private cancelSearch(): void {
    this._searchAbortController?.abort();
    this._searchAbortController = null;
  }

  // ── API helpers ──────────────────────────────────────────────────────────

  private get apiBase(): string {
    switch (this.scope) {
      case 'project':
        return `/api/v1/projects/${this.scopeId}/injected-skills`;
      case 'user':
        return `/api/v1/users/me/injected-skills`;
      case 'hub':
        return `/api/v1/hub/settings/injected-skills`;
    }
  }

  async load(): Promise<void> {
    // Cancel any in-flight load and start fresh — same pattern as _searchAbortController.
    // This handles rapid scope/scopeId changes: the stale request is aborted immediately
    // and only the latest load() commits rows to state.
    this._loadAbortController?.abort();
    this._loadAbortController = new AbortController();
    const { signal } = this._loadAbortController;

    this.loading = true;
    this.error = null;
    try {
      const res = await apiFetch(this.apiBase, { signal });
      if (signal.aborted) return; // Aborted after fetch completed — discard
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      if (this.scope === 'hub') {
        const data = (await res.json()) as {
          system?: Array<{ uri: string; as?: string; optional?: boolean }>;
          user_defined?: Array<{ uri: string; as?: string; optional?: boolean }>;
        };
        if (signal.aborted) return; // Aborted while parsing — discard
        const systemRows: SkillRow[] = (data.system || []).map((s, i) => ({
          id: '',
          uri: s.uri,
          as: s.as || '',
          optional: s.optional ?? false,
          sortOrder: i,
          skillName: '',
          skillSlug: '',
          readonly: true,
        }));
        const userRows: SkillRow[] = (data.user_defined || []).map((s, i) => ({
          id: '',
          uri: s.uri,
          as: s.as || '',
          optional: s.optional ?? false,
          sortOrder: i,
          skillName: '',
          skillSlug: '',
          readonly: false,
        }));
        this.rows = [...systemRows, ...userRows];
      } else {
        const data = (await res.json()) as {
          entries?: Array<{
            id: string;
            skillUri: string;
            skillAs?: string;
            optional?: boolean;
            sortOrder?: number;
            skillName?: string;
            skillSlug?: string;
          }>;
        };
        if (signal.aborted) return; // Aborted while parsing — discard
        this.rows = (data.entries || []).map((e) => ({
          id: e.id,
          uri: e.skillUri,
          as: e.skillAs || '',
          optional: e.optional ?? false,
          sortOrder: e.sortOrder ?? 0,
          skillName: e.skillName || '',
          skillSlug: e.skillSlug || '',
          readonly: false,
        }));
      }
    } catch (err) {
      if (err instanceof Error && err.name === 'AbortError') {
        return; // Silently abort — caller will call load() again with the correct scope
      }
      this.error = err instanceof Error ? err.message : 'Failed to load injected skills';
    } finally {
      if (!signal.aborted) {
        this.loading = false;
      }
    }
  }

  private async addEntry(uri: string, skillAs: string, optional: boolean): Promise<void> {
    if (this.scope === 'hub') {
      // For hub: append to user_defined and PUT the full user_defined list
      const userDefined = this.rows.filter((r) => !r.readonly).map((r) => this.rowToSkillRef(r));
      userDefined.push(this.buildSkillRef(uri, skillAs, optional));
      await this.putHubUserDefined(userDefined);
    } else {
      const res = await apiFetch(this.apiBase, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ skillUri: uri, skillAs: skillAs || undefined, optional }),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Failed to add skill (HTTP ${res.status})`));
      }
    }
    await this.load();
  }

  /**
   * Add several skill URIs at once.
   *
   * URIs already present in this scope are dropped first. Neither backend
   * de-duplicates: the project/user POST endpoint returns 409 on a duplicate
   * skillUri, and the hub PUT stores whatever list it is given, so a
   * re-discovery of the same directory would otherwise either fail outright or
   * append duplicate rows.
   *
   * Hub scope uses a PUT-whole-list API, so calling addEntry() N times would
   * issue N read-modify-write round-trips and leave N-1 pointless intermediate
   * states on the server. Instead we build the full user_defined list once and
   * write it in a single PUT.
   *
   * Project and user scopes have per-item POST endpoints and no batch variant,
   * so they degrade to N individual addEntry() calls. Failures are collected
   * rather than aborting the loop: a mid-batch throw would leave a partial add
   * with no recovery path, since retrying would 409 on the skills that did land.
   */
  private async addEntries(uris: string[]): Promise<void> {
    const existing = new Set(this.rows.map((r) => r.uri));
    const fresh = uris.filter((u) => !existing.has(u));
    // When every selected skill is already present, close silently — no network
    // call needed. Issuing an empty batch would be a no-op PUT on hub scope and
    // zero POSTs elsewhere, so the only effect would be a pointless round-trip.
    if (fresh.length === 0) return;

    if (this.scope === 'hub') {
      const userDefined = this.rows.filter((r) => !r.readonly).map((r) => this.rowToSkillRef(r));
      for (const uri of fresh) {
        userDefined.push(this.buildSkillRef(uri, '', false));
      }
      await this.putHubUserDefined(userDefined);
      await this.load();
      return;
    }

    const failures: string[] = [];
    for (const uri of fresh) {
      try {
        await this.addEntry(uri, '', false);
      } catch (err) {
        failures.push(`${uri}: ${err instanceof Error ? err.message : 'failed'}`);
      }
    }
    if (failures.length > 0) {
      throw new Error(
        `${failures.length} of ${fresh.length} skills could not be added — ${failures.join('; ')}`
      );
    }
  }

  private async deleteEntry(row: SkillRow, rowIndex: number): Promise<void> {
    if (this.scope === 'hub') {
      // For hub: remove from user_defined and PUT.
      // Filter by index (not URI) so duplicate URIs don't cause silent double-deletion.
      const userDefined = this.rows
        .filter((r, i) => !r.readonly && i !== rowIndex)
        .map((r) => this.rowToSkillRef(r));
      await this.putHubUserDefined(userDefined);
    } else {
      const res = await apiFetch(`${this.apiBase}/${encodeURIComponent(row.id)}`, {
        method: 'DELETE',
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Failed to delete skill (HTTP ${res.status})`));
      }
    }
    await this.load();
  }

  private async reorder(newOrder: SkillRow[]): Promise<void> {
    if (this.scope === 'hub') {
      const userDefined = newOrder.filter((r) => !r.readonly).map((r) => this.rowToSkillRef(r));
      await this.putHubUserDefined(userDefined);
    } else {
      const entries = newOrder.map((r, i) => ({
        id: r.id,
        skillUri: r.uri,
        skillAs: r.as || undefined,
        optional: r.optional,
        sortOrder: i + 1,
      }));
      const res = await apiFetch(this.apiBase, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ entries }),
      });
      if (!res.ok) {
        throw new Error(
          await extractApiError(res, `Failed to reorder skills (HTTP ${res.status})`)
        );
      }
    }
    await this.load();
  }

  /** Convert a SkillRow to the SkillReference wire format for hub PUT. */
  private rowToSkillRef(r: SkillRow): { uri: string; as?: string; optional?: boolean } {
    return this.buildSkillRef(r.uri, r.as, r.optional);
  }

  /** Build a SkillReference object, omitting undefined optional fields. */
  private buildSkillRef(
    uri: string,
    as: string,
    optional: boolean
  ): { uri: string; as?: string; optional?: boolean } {
    const ref: { uri: string; as?: string; optional?: boolean } = { uri };
    if (as) ref.as = as;
    if (optional) ref.optional = true;
    return ref;
  }

  // Note: hub-scope skill injection uses a PUT-whole-list API (no per-item DELETE endpoint).
  // Concurrent deletes can cause a lost-update race: if two deletes are in flight simultaneously,
  // the second PUT will overwrite the first. This is an architectural limitation of the hub API
  // and should be addressed by adding a per-item DELETE endpoint in a future change.
  private async putHubUserDefined(
    userDefined: Array<{ uri: string; as?: string; optional?: boolean }>
  ): Promise<void> {
    const res = await apiFetch(this.apiBase, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ user_defined: userDefined }),
    });
    if (!res.ok) {
      throw new Error(
        await extractApiError(res, `Failed to update hub skills (HTTP ${res.status})`)
      );
    }
  }

  // ── Dialog ────────────────────────────────────────────────────────────────

  private openDialog(): void {
    this.dialogMode = 'search';
    this.dialogSkillQuery = '';
    this.dialogSkillResults = [];
    this.dialogSkillSearching = false;
    this.dialogSelectedSkill = null;
    this.dialogUri = '';
    this.dialogAs = '';
    this.dialogOptional = false;
    this.dialogLoading = false;
    this.dialogError = null;
    this.dialogTransformed = null;
    this.resetDiscovery();
    this.dialogOpen = true;
  }

  /**
   * Clear all directory-discovery state. Called from both openDialog() and
   * closeDialog() so a discovery error or a stale selection can never survive a
   * Cancel and reappear the next time the add dialog is opened.
   */
  private resetDiscovery(): void {
    this.discoveryDialogOpen = false;
    this.discoveredSkills = [];
    this.skippedSkillNames = [];
    this.selectedSkillURIs = new Set();
    this.discoveryLoading = false;
    this.discoveryError = null;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
    if (this.searchTimer) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
    // Abort any in-flight search so it doesn't update state after close.
    this.cancelSearch();
    this.resetDiscovery();
  }

  private handleSearchInput(query: string): void {
    this.dialogSkillQuery = query;
    this.dialogSelectedSkill = null;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    if (!query.trim()) {
      this.cancelSearch();
      this.dialogSkillSearching = false;
      this.dialogSkillResults = [];
      return;
    }
    this.dialogSkillSearching = true;
    this.searchTimer = setTimeout(() => void this.searchSkills(query), 300);
  }

  private async searchSkills(query: string): Promise<void> {
    this._searchAbortController?.abort();
    this._searchAbortController = new AbortController();
    const { signal } = this._searchAbortController;
    try {
      const res = await apiFetch(
        `/api/v1/skills?q=${encodeURIComponent(query)}&status=active&limit=20`,
        { signal }
      );
      if (res.ok) {
        const data = (await res.json()) as { skills?: Skill[] } | Skill[];
        this.dialogSkillResults = Array.isArray(data)
          ? data
          : (data as { skills?: Skill[] }).skills || [];
      }
    } catch (err) {
      if (err instanceof Error && err.name === 'AbortError') return; // Stale request — discard
      // Non-critical — just show no results
    } finally {
      // Only clear the spinner if this request was not aborted — a newer in-flight
      // request (or closeDialog) may have aborted it and its spinner is still needed.
      if (!signal.aborted) {
        this.dialogSkillSearching = false;
      }
    }
  }

  private async handleAddSkill(e: Event): Promise<void> {
    e.preventDefault();

    let uri = '';
    if (this.dialogMode === 'search') {
      if (!this.dialogSelectedSkill) {
        this.dialogError = 'Please select a skill from the search results';
        return;
      }
      // Build a canonical skill bank URI: skill://scion/<slug>
      // A single-segment form (skill://<slug>) is now accepted by
      // ParseSkillURI, but the two-segment form is the canonical stored
      // shape and avoids ambiguity with the registry field.
      uri = `skill://scion/${this.dialogSelectedSkill.slug}`;
    } else {
      const raw = this.dialogUri.trim();
      if (!raw) {
        this.dialogError = 'Skill URI is required';
        return;
      }
      // Normalize client-side before sending; the hub is authoritative for edge cases.
      try {
        const result = normalizeSkillURIClient(raw);
        uri = result.canonical;
      } catch (normErr) {
        this.dialogError = normErr instanceof Error ? normErr.message : 'Invalid skill URI';
        return;
      }
    }

    this.dialogLoading = true;
    this.dialogError = null;
    try {
      await this.addEntry(uri, this.dialogAs.trim(), this.dialogOptional);
      this.closeDialog();
    } catch (err) {
      this.dialogError = err instanceof Error ? err.message : 'Failed to add skill';
    } finally {
      this.dialogLoading = false;
    }
  }

  // ── Directory discovery (batch add) ──────────────────────────────────────

  /**
   * True when the URI currently typed in the dialog should offer directory
   * discovery. A URL that normalizes to a gh:// or skill:// URI is
   * unambiguously a single skill, so only "Add Skill" is offered. A URL that
   * stays a full https:// URL either points at a directory of skills or at a
   * single skill on a non-standard path — the user's choice of button
   * disambiguates.
   *
   * The github.com host is required because the discover endpoint rejects
   * anything else; offering the button for other hosts would only buy the user
   * a round-trip to a 400.
   */
  private get showDiscoverButton(): boolean {
    const candidate = this.dialogTransformed ?? this.dialogUri.trim();
    return candidate.toLowerCase().startsWith('https://github.com/');
  }

  /**
   * Probe the pasted URL for skills. On success this opens the selection
   * dialog with everything pre-selected; on failure the error is shown inline
   * in the add dialog and no selection dialog is opened.
   */
  private async handleDiscoverDirectory(): Promise<void> {
    // Post the raw directory URL, not the normalized form — discover needs the directory path.
    const sourceUrl = this.dialogUri.trim();
    if (!sourceUrl) {
      this.discoveryError = 'Skill URI is required';
      return;
    }

    this.discoveryLoading = true;
    this.discoveryError = null;
    this.dialogError = null;
    try {
      const body: { sourceUrl: string; projectId?: string } = { sourceUrl };
      if (this.scope === 'project' && this.scopeId) body.projectId = this.scopeId;

      const res = await apiFetch('/api/v1/skills/discover-directory', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Discovery failed (HTTP ${res.status})`));
      }
      const data = (await res.json()) as {
        skills?: Array<{ uri: string; name: string }>;
        skipped?: string[];
      };
      this.discoveredSkills = data.skills ?? [];
      this.skippedSkillNames = data.skipped ?? [];
      if (this.discoveredSkills.length === 0) {
        this.discoveryError = 'No skills found at this URL.';
        return;
      }
      this.selectedSkillURIs = new Set(this.discoveredSkills.map((s) => s.uri));
      this.discoveryDialogOpen = true;
    } catch (err) {
      this.discoveryError = err instanceof Error ? err.message : 'Discovery failed';
    } finally {
      this.discoveryLoading = false;
    }
  }

  /** Add every checked skill, then close both the selection and add dialogs. */
  private async handleDiscoveryConfirm(): Promise<void> {
    this.discoveryLoading = true;
    this.discoveryError = null;
    try {
      await this.addEntries([...this.selectedSkillURIs]);
      this.discoveryDialogOpen = false;
      // closeDialog() clears discovery state via resetDiscovery().
      this.closeDialog();
    } catch (err) {
      this.discoveryError = err instanceof Error ? err.message : 'Failed to add selected skills';
    } finally {
      this.discoveryLoading = false;
    }
  }

  /**
   * Checkbox list of discovered skills. Rows show the bare directory name; no
   * SKILL.md frontmatter is read, so there is no description to display.
   *
   * This deliberately duplicates the select-all/list/count shape of
   * resource-import.ts rather than sharing a component: the two dialogs operate
   * on different data shapes and the duplication is ~70 lines.
   */
  private renderDiscoveryDialog() {
    if (!this.discoveryDialogOpen) return nothing;

    const total = this.discoveredSkills.length;
    const allSelected = this.selectedSkillURIs.size === total;
    const noneSelected = this.selectedSkillURIs.size === 0;

    return html`
      <sl-dialog
        label="Select Skills to Add"
        open
        @sl-request-close=${() => {
          this.discoveryDialogOpen = false;
        }}
        style="--width: 560px;"
      >
        <div class="selection-header">
          <sl-checkbox
            ?checked=${allSelected}
            ?indeterminate=${!allSelected && !noneSelected}
            @sl-change=${(e: Event) => {
              const checked = (e.target as HTMLInputElement).checked;
              this.selectedSkillURIs = checked
                ? new Set(this.discoveredSkills.map((s) => s.uri))
                : new Set();
            }}
          >
            Select All
          </sl-checkbox>
          <span class="selection-count">${this.selectedSkillURIs.size} of ${total} selected</span>
        </div>
        <div class="selection-list">
          ${this.discoveredSkills.map(
            (skill) => html`
              <div class="selection-item">
                <sl-checkbox
                  ?checked=${this.selectedSkillURIs.has(skill.uri)}
                  @sl-change=${(e: Event) => {
                    const checked = (e.target as HTMLInputElement).checked;
                    const updated = new Set(this.selectedSkillURIs);
                    if (checked) updated.add(skill.uri);
                    else updated.delete(skill.uri);
                    this.selectedSkillURIs = updated;
                  }}
                >
                  ${skill.name}
                </sl-checkbox>
              </div>
            `
          )}
        </div>
        ${this.skippedSkillNames.length > 0
          ? html`<p class="discovery-skipped-note">
              ${this.skippedSkillNames.length}
              folder${this.skippedSkillNames.length === 1 ? '' : 's'} not recognized as skills
              ${this.skippedSkillNames.length === 1 ? 'was' : 'were'} skipped.
            </p>`
          : nothing}
        ${this.discoveryError
          ? html`<div class="dialog-error">${this.discoveryError}</div>`
          : nothing}

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.discoveryLoading}
          @click=${() => {
            this.discoveryDialogOpen = false;
          }}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.discoveryLoading}
          ?disabled=${noneSelected || this.discoveryLoading}
          @click=${() => void this.handleDiscoveryConfirm()}
        >
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add Selected (${this.selectedSkillURIs.size})
        </sl-button>
      </sl-dialog>
    `;
  }

  // ── Drag & drop ──────────────────────────────────────────────────────────

  private handleDragStart(index: number, e: DragEvent): void {
    this.dragSourceIndex = index;
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = 'move';
    }
  }

  private handleDragOver(index: number, e: DragEvent): void {
    const targetRow = this.rows[index];
    if (targetRow?.readonly) {
      // Readonly rows reject drops — no highlight, no cursor change, no position update.
      // Do NOT call e.preventDefault() so the browser keeps its default "no-drop" cursor.
      return;
    }
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
    this.dragOverIndex = index;
  }

  private handleDragLeave(): void {
    this.dragOverIndex = null;
  }

  private handleDrop(targetIndex: number, e: DragEvent): void {
    e.preventDefault();
    this.dragOverIndex = null;
    if (this.dragSourceIndex === null || this.dragSourceIndex === targetIndex) {
      this.dragSourceIndex = null;
      return;
    }
    const sourceIndex = this.dragSourceIndex;
    const newOrder = [...this.rows];
    const [moved] = newOrder.splice(sourceIndex, 1);
    // For drag-down (source < target), removing the source shifts all elements
    // after it up by 1. targetIndex now points one slot past the intended drop
    // position (the element AFTER the intended target). Subtract 1 to correct.
    // For drag-up (source > target), indices are unaffected — no adjustment.
    const insertAt = sourceIndex < targetIndex ? targetIndex - 1 : targetIndex;
    newOrder.splice(insertAt, 0, moved);
    this.dragSourceIndex = null;
    // Optimistic update
    this.rows = newOrder;
    void this.reorder(newOrder).catch((err) => {
      console.error('Reorder failed:', err);
      // Revert optimistic update; if reload also fails, surface an error message.
      void this.load().catch((reloadErr) => {
        console.error('Failed to reload after reorder error:', reloadErr);
        this.error = 'Failed to reload — please refresh';
      });
    });
  }

  private handleDragEnd(): void {
    this.dragSourceIndex = null;
    this.dragOverIndex = null;
  }

  // ── Rendering ────────────────────────────────────────────────────────────

  override render() {
    const canEdit = !this.readonly;

    return html`
      <div class="panel-header">
        <div class="panel-header-info">
          <p>${this.renderDescription()}</p>
        </div>
        ${canEdit
          ? html`
              <sl-button variant="primary" size="small" @click=${this.openDialog}>
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Add Skill
              </sl-button>
            `
          : nothing}
      </div>

      ${this.loading
        ? html`<div class="section-loading"><sl-spinner></sl-spinner> Loading skills…</div>`
        : this.error
          ? html`
              <div class="section-error">
                <span>${this.error}</span>
                <sl-button size="small" @click=${() => this.load()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </div>
            `
          : this.rows.length === 0
            ? this.renderEmpty()
            : this.renderTable()}
      ${this.renderDialog()} ${this.renderDiscoveryDialog()}
    `;
  }

  /**
   * Renders a skill URI with middle-truncation: the prefix (scheme + path)
   * truncates with an ellipsis when space is tight, while the skill name
   * (last path segment) is always fully visible.
   */
  private renderMiddleTruncatedUri(uri: string) {
    if (!uri) {
      return nothing;
    }
    const lastSlash = uri.lastIndexOf('/');
    if (lastSlash === -1 || lastSlash === uri.length - 1) {
      // No path separator or trailing slash — show the whole URI as-is
      // (still needs nowrap to avoid wrapping in the flex container)
      return html`<span class="skill-uri" title=${uri}
        ><span class="skill-uri-name">${uri}</span></span
      >`;
    }
    const prefix = uri.slice(0, lastSlash + 1);
    const name = uri.slice(lastSlash + 1);
    return html`<span class="skill-uri" title=${uri}
      ><span class="skill-uri-prefix">${prefix}</span
      ><span class="skill-uri-name">${name}</span></span
    >`;
  }

  private renderDescription(): string {
    switch (this.scope) {
      case 'project':
        return 'Skills automatically injected into every agent in this project.';
      case 'user':
        return 'Skills automatically injected into every agent you own, across all projects.';
      case 'hub':
        return 'Skills automatically injected into all agents on this hub.';
    }
  }

  private renderEmpty() {
    const canEdit = !this.readonly;
    return html`
      <div class="empty-state">
        <sl-icon name="puzzle"></sl-icon>
        <h3>No Injected Skills</h3>
        <p>${this.renderDescription()}</p>
        ${canEdit
          ? html`
              <sl-button variant="primary" size="small" @click=${this.openDialog}>
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Add Skill
              </sl-button>
            `
          : nothing}
      </div>
    `;
  }

  private renderTable() {
    const canEdit = !this.readonly;
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              ${canEdit ? html`<th style="width: 2rem;"></th>` : nothing}
              <th>Skill</th>
              <th>Alias (as)</th>
              <th>Optional</th>
              ${canEdit ? html`<th class="actions-cell"></th>` : nothing}
            </tr>
          </thead>
          <tbody>
            ${this.rows.map((row, index) => this.renderRow(row, index, canEdit))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(row: SkillRow, index: number, canEdit: boolean) {
    const isDeleting = this._deletingIndex === index;
    const isDragging = this.dragSourceIndex === index;
    const isDragOver = this.dragOverIndex === index;
    const rowReadonly = row.readonly;

    // A row can be dragged only if it's not readonly and we have a skill with an id
    const draggable = canEdit && !rowReadonly;

    return html`
      <tr
        class=${[isDragging ? 'dragging' : '', isDragOver ? 'drag-over' : ''].join(' ')}
        draggable=${draggable ? 'true' : 'false'}
        @dragstart=${draggable ? (e: DragEvent) => this.handleDragStart(index, e) : nothing}
        @dragover=${canEdit ? (e: DragEvent) => this.handleDragOver(index, e) : nothing}
        @dragleave=${canEdit ? () => this.handleDragLeave() : nothing}
        @drop=${(e: DragEvent) => this.handleDrop(index, e)}
        @dragend=${draggable ? () => this.handleDragEnd() : nothing}
      >
        ${canEdit
          ? html`
              <td style="width: 2rem; padding: 0.5rem;">
                ${rowReadonly
                  ? nothing
                  : html`<span class="drag-handle" title="Drag to reorder">⠿</span>`}
              </td>
            `
          : nothing}
        <td>
          <div class="skill-info">
            ${row.skillName ? html`<span class="skill-name">${row.skillName}</span>` : nothing}
            ${this.renderMiddleTruncatedUri(row.uri)}
            ${row.skillSlug ? html`<span class="skill-uri">/${row.skillSlug}</span>` : nothing}
          </div>
          ${rowReadonly
            ? html`
                <span class="system-badge" style="margin-top: 0.25rem; display: inline-flex;">
                  <sl-icon name="lock"></sl-icon>
                  System
                </span>
              `
            : nothing}
        </td>
        <td>
          ${row.as
            ? html`<span class="key-cell">${row.as}</span>`
            : html`<span style="color: var(--scion-text-muted, #64748b); font-size: 0.8125rem;"
                >—</span
              >`}
        </td>
        <td>
          ${row.optional
            ? html`<sl-icon
                name="check-circle"
                style="color: var(--sl-color-success-600, #16a34a);"
              ></sl-icon>`
            : html`<sl-icon
                name="x-circle"
                style="color: var(--scion-text-muted, #64748b);"
              ></sl-icon>`}
        </td>
        ${canEdit
          ? html`
              <td class="actions-cell">
                ${rowReadonly
                  ? nothing
                  : html`
                      <sl-icon-button
                        name="trash"
                        label="Remove"
                        ?disabled=${isDeleting}
                        @click=${() => this.handleDeleteRow(row, index)}
                        style="color: var(--sl-color-danger-600, #dc2626);"
                      ></sl-icon-button>
                    `}
              </td>
            `
          : nothing}
      </tr>
    `;
  }

  private async handleDeleteRow(row: SkillRow, rowIndex: number): Promise<void> {
    const label = row.skillName || row.uri;
    if (
      !(await showConfirm(
        `Remove skill "${label}" from this ${this.scope === 'hub' ? 'hub' : this.scope === 'project' ? 'project' : 'profile'}?`
      ))
    ) {
      return;
    }
    // Guard against stale rowIndex: a concurrent drag-reorder between the click
    // and the showConfirm() call could shift row positions. Re-find the row by its
    // stable identity (URI + id) before committing the delete.
    let resolvedIndex = rowIndex;
    const currentAtIndex = this.rows[rowIndex];
    if (!currentAtIndex || currentAtIndex.uri !== row.uri || currentAtIndex.id !== row.id) {
      const found = this.rows.findIndex((r) => r.uri === row.uri && r.id === row.id);
      if (found === -1) return; // Row no longer present — cancel delete
      resolvedIndex = found;
    }
    this._deletingIndex = resolvedIndex;
    try {
      await this.deleteEntry(row, resolvedIndex);
    } catch (err) {
      console.error('Failed to delete skill:', err);
      showToast(err instanceof Error ? err.message : 'Failed to remove skill');
    } finally {
      this._deletingIndex = null;
    }
  }

  private renderDialog() {
    const isSearchMode = this.dialogMode === 'search';

    return html`
      <sl-dialog
        label="Add Injected Skill"
        ?open=${this.dialogOpen}
        @sl-request-close=${this.closeDialog}
        style="--width: 560px;"
      >
        <div class="dialog-form">
          <div class="mode-toggle">
            <sl-button
              size="small"
              variant=${isSearchMode ? 'primary' : 'default'}
              @click=${() => {
                this.dialogMode = 'search';
                this.dialogError = null;
              }}
            >
              <sl-icon slot="prefix" name="search"></sl-icon>
              Skill Bank
            </sl-button>
            <sl-button
              size="small"
              variant=${!isSearchMode ? 'primary' : 'default'}
              @click=${() => {
                this.dialogMode = 'uri';
                this.dialogError = null;
              }}
            >
              <sl-icon slot="prefix" name="link-45deg"></sl-icon>
              External URI
            </sl-button>
          </div>

          ${isSearchMode
            ? html`
                <sl-input
                  label="Search skills"
                  placeholder="Type to search…"
                  clearable
                  .value=${this.dialogSkillQuery}
                  @sl-input=${(e: Event) =>
                    this.handleSearchInput((e.target as HTMLInputElement).value)}
                >
                  ${this.dialogSkillSearching
                    ? html`<sl-spinner slot="suffix" style="font-size: 1rem;"></sl-spinner>`
                    : nothing}
                </sl-input>

                ${this.dialogSkillResults.length > 0
                  ? html`
                      <div class="search-results">
                        ${this.dialogSkillResults.map(
                          (skill) => html`
                            <div
                              class="search-result-item ${this.dialogSelectedSkill?.id === skill.id
                                ? 'selected'
                                : ''}"
                              @click=${() => {
                                this.dialogSelectedSkill = skill;
                              }}
                            >
                              <sl-icon
                                name="puzzle"
                                style="color: var(--scion-primary, #3b82f6);"
                              ></sl-icon>
                              <div>
                                <div>${skill.name}</div>
                                <div class="skill-slug">${skill.slug}</div>
                              </div>
                              ${this.dialogSelectedSkill?.id === skill.id
                                ? html`<sl-icon
                                    name="check-circle-fill"
                                    style="margin-left: auto; color: var(--scion-primary, #3b82f6);"
                                  ></sl-icon>`
                                : nothing}
                            </div>
                          `
                        )}
                      </div>
                    `
                  : this.dialogSkillQuery && !this.dialogSkillSearching
                    ? html`<div class="no-results">
                        No skills found matching "${this.dialogSkillQuery}"
                      </div>`
                    : nothing}
                ${this.dialogSelectedSkill
                  ? html`
                      <div class="dialog-hint">
                        <sl-icon name="check-circle"></sl-icon>
                        Selected: <strong>${this.dialogSelectedSkill.name}</strong>
                        (${this.dialogSelectedSkill.slug})
                      </div>
                    `
                  : nothing}
              `
            : html`
                <sl-input
                  label="Skill URI"
                  placeholder="e.g. skill://scion/core/my-skill or https://github.com/org/repo/tree/main/skills/my-skill"
                  help-text="Hub-skill URI (skill://…), GitHub tree or blob URL (auto-transformed), or gh:// shorthand."
                  .value=${this.dialogUri}
                  @sl-input=${(e: Event) => {
                    const val = (e.target as HTMLInputElement).value;
                    this.dialogUri = val;
                    // Live transform preview: attempt normalization on each keystroke.
                    try {
                      const result = normalizeSkillURIClient(val);
                      this.dialogTransformed = result.transformed ? result.canonical : null;
                    } catch {
                      this.dialogTransformed = null;
                    }
                  }}
                ></sl-input>
                ${this.dialogTransformed
                  ? html`<div class="dialog-hint">
                      <sl-icon name="arrow-right-circle"></sl-icon>
                      Will be stored as: <strong>${this.dialogTransformed}</strong>
                    </div>`
                  : nothing}
                ${this.showDiscoverButton
                  ? html`<div class="dialog-hint">
                      <sl-icon name="folder2-open"></sl-icon>
                      This looks like it could be a directory of skills — use
                      <strong>Discover Skills from Directory</strong> to pick several at once.
                    </div>`
                  : nothing}
                ${this.discoveryError
                  ? html`<div class="dialog-error">${this.discoveryError}</div>`
                  : nothing}
              `}

          <sl-input
            label="Alias (as)"
            placeholder="Optional — override the skill's default name"
            help-text="If set, the skill will be installed under this name instead of its default."
            .value=${this.dialogAs}
            @sl-input=${(e: Event) => {
              this.dialogAs = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <label class="checkbox-label">
            <input
              type="checkbox"
              .checked=${this.dialogOptional}
              @change=${(e: Event) => {
                this.dialogOptional = (e.target as HTMLInputElement).checked;
              }}
            />
            <span class="checkbox-text">
              <span>Optional</span>
              <span class="checkbox-description">
                If checked, a resolution failure for this skill will log a warning but not fail
                agent provisioning.
              </span>
            </span>
          </label>

          ${this.dialogError ? html`<div class="dialog-error">${this.dialogError}</div>` : nothing}
        </div>

        <sl-button
          slot="footer"
          variant="default"
          @click=${this.closeDialog}
          ?disabled=${this.dialogLoading || this.discoveryLoading}
        >
          Cancel
        </sl-button>
        ${!isSearchMode && this.showDiscoverButton
          ? html`
              <sl-button
                slot="footer"
                variant="default"
                ?loading=${this.discoveryLoading}
                ?disabled=${this.dialogLoading || this.discoveryLoading}
                @click=${() => void this.handleDiscoverDirectory()}
              >
                <sl-icon slot="prefix" name="folder2-open"></sl-icon>
                Discover Skills from Directory
              </sl-button>
            `
          : nothing}
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.dialogLoading}
          ?disabled=${this.dialogLoading || this.discoveryLoading}
          @click=${this.handleAddSkill}
        >
          Add Skill
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-injected-skills-panel': ScionInjectedSkillsPanel;
  }
}
