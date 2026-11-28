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
 * Trigger Rules list & authoring tab (Stage 2 §9.1, GAR G7 extension points,
 * P9 action-array readiness).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { paginateAll } from '../../client/paginate-all.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { resourceStyles } from '../shared/resource-styles.js';
import {
  ADMITTED_TRIGGER_DIALECTS,
  fireStatusBadgeVariant,
  formatRedactedExecSummary,
  getTriggerActions,
  parseTriggerRuleRowErrors,
  type ListTriggerEventsResponse,
  type ListTriggersResponse,
  type ProjectAgentOption,
  type TriggerActionSpec,
  type TriggerDefinition,
  type TriggerMatchRuleSpec,
  type TriggerNotifySpec,
  type TriggerRenderResponse,
  type TriggerRenderSlotResult,
  type TriggerRuleRowError,
  type WebhookEndpoint,
} from './types.js';
import {
  ensureExtensionLoaded,
  getFormAuthoring,
  getRegisteredSourceKinds,
  getSourceKind,
  type FormAuthoringOutput,
  type SourceIdMode,
} from './source-extensions.js';
import { createDefaultMessageAction } from './action-editor.js';
import {
  DEFAULT_NOTIFY_OUTCOMES,
  createDefaultNotifySpec,
  normalizeNotifySpec,
} from './notify-editor.js';
import './where-editor.js';
import './action-editor.js';
import './notify-editor.js';

@customElement('scion-trigger-list')
export class ScionTriggerList extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: String })
  projectId = '';

  @property({ type: Array })
  endpoints: WebhookEndpoint[] = [];

  @property({ type: Array })
  agents: ProjectAgentOption[] = [];

  /** Deep-link request to open the create dialog for a specific source kind. */
  @property({ type: String })
  pendingCreateSource = '';

  /** Deep-link preset ID passed to a registered source form-authoring pane. */
  @property({ type: String })
  pendingCreatePreset = '';

  /**
   * Multi-action support (P9, ptone/scion#9065 / ptone/scion#9785): sends
   * `actions[]` on POST /triggers and PATCH /triggers/{id}.
   */
  @property({ type: Boolean })
  multiActionEnabled = true;

  @state() private triggers: TriggerDefinition[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private expandedIds = new Set<string>();

  // Create / Edit dialog state
  @state() private dialogOpen = false;
  @state() private editingTrigger: TriggerDefinition | null = null;
  @state() private saving = false;
  @state() private formError: string | null = null;
  @state() private whereRowErrors: Record<number, TriggerRuleRowError> = {};

  @state() private formName = '';
  @state() private formDescription = '';
  @state() private formSourceKind = 'webhook';
  @state() private formSourceId = '';
  @state() private formTypes: string[] = ['webhook.received'];
  @state() private formTypeDraft = '';
  @state() private formRules: TriggerMatchRuleSpec[] = [];
  @state() private formMatchForm: unknown = undefined;
  @state() private formMatchFormDropped = false;
  @state() private formActions: TriggerActionSpec[] = [createDefaultMessageAction()];
  @state() private formSubjectMode = 'new';
  @state() private formSubjectExpr = '';
  @state() private formCoalesceWindow = '';
  @state() private formMaxFiresPerMinute = '';
  @state() private formMaxConcurrentDispatches = '';
  @state() private formNotify: TriggerNotifySpec = createDefaultNotifySpec();
  @state() private formAuthoringPane: 'form' | 'cel' = 'cel';
  @state() private formAuthoringValid = true;
  @state() private formAuthoringErrors: string[] = [];
  @state() private formPresetId = '';

  // Preview (render) dialog state
  @state() private previewDialogOpen = false;
  @state() private previewTrigger: TriggerDefinition | null = null;
  @state() private previewLoading = false;
  @state() private previewError: string | null = null;
  @state() private previewEventId = '';
  @state() private previewResult: TriggerRenderResponse | null = null;

  // Delete confirm dialog state
  @state() private deleteTarget: TriggerDefinition | null = null;
  @state() private deleting = false;

  private handledDeepLink = false;

  static override styles = [
    resourceStyles,
    css`
      .toolbar {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.75rem;
        margin-bottom: 1rem;
      }

      .rule-name {
        font-weight: 600;
        color: var(--scion-text);
      }

      .rule-meta {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
      }

      .actions-cell {
        white-space: nowrap;
      }

      .expand-row td {
        background: var(--scion-bg-subtle);
        padding: 1rem 1.25rem;
      }

      .detail-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
        gap: 0.75rem;
        font-size: 0.8125rem;
      }

      .detail-label {
        font-size: 0.75rem;
        font-weight: 600;
        color: var(--scion-text-muted);
        text-transform: uppercase;
        letter-spacing: 0.04em;
        margin-bottom: 0.25rem;
      }

      .mono {
        font-family: var(--scion-font-mono);
        font-size: 0.8125rem;
      }

      .dialog-form {
        display: flex;
        flex-direction: column;
        gap: 0.875rem;
      }

      .field-grid {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 0.75rem;
      }

      .chips-row {
        display: flex;
        flex-wrap: wrap;
        gap: 0.375rem;
        align-items: center;
      }

      .chip-input-row {
        display: flex;
        gap: 0.5rem;
        align-items: flex-end;
      }

      .chip-input-row sl-input {
        flex: 1;
      }

      .section-divider {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text-muted);
        border-top: 1px solid var(--scion-border);
        padding-top: 0.75rem;
      }

      .preview-slot {
        padding: 0.75rem;
        background: var(--scion-bg-subtle);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius);
        margin-bottom: 0.75rem;
      }

      .preview-slot pre {
        margin: 0.375rem 0 0 0;
        white-space: pre-wrap;
        word-break: break-word;
        font-family: var(--scion-font-mono);
        font-size: 0.8125rem;
      }

      .var-badges {
        display: flex;
        flex-wrap: wrap;
        gap: 0.25rem;
        margin-top: 0.5rem;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.projectId) {
      void this.loadTriggers();
    }
    this.maybeOpenPendingCreate();
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('projectId') && this.projectId) {
      void this.loadTriggers();
    }
    if (changed.has('pendingCreateSource') || changed.has('pendingCreatePreset')) {
      this.handledDeepLink = false;
      this.maybeOpenPendingCreate();
    }
  }

  private maybeOpenPendingCreate(): void {
    if (this.handledDeepLink || !this.pendingCreateSource) return;
    this.handledDeepLink = true;
    this.openCreateDialog(this.pendingCreateSource, this.pendingCreatePreset);
  }

  async loadTriggers(): Promise<void> {
    if (!this.projectId) return;
    this.loading = true;
    this.error = null;
    try {
      const items = await paginateAll<TriggerDefinition>({
        path: `/api/v1/projects/${encodeURIComponent(this.projectId)}/triggers`,
        pageSize: 100,
        parsePage: (raw) => {
          const data = raw as ListTriggersResponse;
          const pageItems = Array.isArray(data.triggers) ? data.triggers : [];
          return data.nextCursor
            ? { items: pageItems, nextCursor: data.nextCursor }
            : { items: pageItems };
        },
      });
      this.triggers = items;
      this.dispatchEvent(
        new CustomEvent<{ triggers: TriggerDefinition[] }>('triggers-loaded', {
          detail: { triggers: items },
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load triggers';
    } finally {
      this.loading = false;
    }
  }

  private sourceIdModeFor(kind: string): SourceIdMode {
    if (kind === 'webhook') return 'endpoint';
    const reg = getSourceKind(kind);
    if (reg) return reg.sourceIdMode;
    // Unregistered non-webhook kinds default to 'none' (empty source.id) so
    // app-shaped sources work even if only form-authoring was registered.
    return 'none';
  }

  private handleSourceKindChange(nextKind: string): void {
    const kind = nextKind.trim() || 'webhook';
    this.formSourceKind = kind;
    const mode = this.sourceIdModeFor(kind);
    if (mode === 'none') {
      this.formSourceId = '';
    } else if (mode === 'endpoint' && !this.formSourceId) {
      this.formSourceId = this.endpoints[0]?.id ?? '';
    }
    if (!this.editingTrigger) {
      const reg = getSourceKind(kind);
      if (kind === 'webhook') {
        this.formTypes = ['webhook.received'];
      } else if (reg?.defaultTypes) {
        this.formTypes = [...reg.defaultTypes];
      } else {
        this.formTypes = [];
      }
    }
    const kindReg = getSourceKind(kind);
    if (kindReg?.load) {
      void ensureExtensionLoaded(kindReg)
        .then(() => this.requestUpdate())
        .catch(() => {
          // Evicted on failure; retryable.
        });
    }
    const ext = getFormAuthoring(kind);
    this.formAuthoringPane = ext ? 'form' : 'cel';
    this.formAuthoringValid = true;
    this.formAuthoringErrors = [];
    if (ext) {
      void ensureExtensionLoaded(ext)
        .then(() => this.requestUpdate())
        .catch(() => {
          // Evicted on failure; retryable.
        });
    }
  }

  openCreateDialog(sourceKind = 'webhook', presetId = ''): void {
    this.editingTrigger = null;
    this.formName = '';
    this.formDescription = '';
    this.formSourceKind = sourceKind || 'webhook';
    const mode = this.sourceIdModeFor(this.formSourceKind);
    this.formSourceId = mode === 'endpoint' ? (this.endpoints[0]?.id ?? '') : '';
    const kindReg = getSourceKind(this.formSourceKind);
    this.formTypes =
      this.formSourceKind === 'webhook'
        ? ['webhook.received']
        : kindReg?.defaultTypes
          ? [...kindReg.defaultTypes]
          : [];
    this.formTypeDraft = '';
    this.formRules = [];
    this.formMatchForm = undefined;
    this.formMatchFormDropped = false;
    this.formActions = [createDefaultMessageAction()];
    this.formSubjectMode = 'new';
    this.formSubjectExpr = '';
    this.formCoalesceWindow = '';
    this.formMaxFiresPerMinute = '';
    this.formMaxConcurrentDispatches = '';
    this.formNotify = createDefaultNotifySpec();
    this.formPresetId = presetId;
    this.formError = null;
    this.whereRowErrors = {};
    this.formAuthoringValid = true;
    this.formAuthoringErrors = [];

    if (kindReg?.load) {
      void ensureExtensionLoaded(kindReg)
        .then(() => this.requestUpdate())
        .catch(() => {
          // Evicted on failure; retryable.
        });
    }
    const formExt = getFormAuthoring(this.formSourceKind);
    this.formAuthoringPane = formExt ? 'form' : 'cel';
    if (formExt) {
      void ensureExtensionLoaded(formExt)
        .then(() => this.requestUpdate())
        .catch(() => {
          // Evicted on failure; retryable.
        });
    }
    this.dialogOpen = true;
  }

  private openEditDialog(tr: TriggerDefinition): void {
    this.editingTrigger = tr;
    this.formName = tr.name;
    this.formDescription = tr.description ?? '';
    this.formSourceKind = tr.source.kind;
    this.formSourceId = tr.source.id;
    this.formTypes = [...(tr.match.types ?? [])];
    this.formTypeDraft = '';
    this.formRules = (tr.match.rules ?? []).map((r) => ({ ...r }));
    this.formMatchForm = tr.match.form;
    this.formMatchFormDropped = false;
    const actions = getTriggerActions(tr);
    this.formActions =
      actions.length > 0 ? actions.map((a) => ({ ...a })) : [createDefaultMessageAction()];
    this.formSubjectMode = tr.subject.mode || 'new';
    this.formSubjectExpr = tr.subject.expr ?? '';
    this.formCoalesceWindow = tr.subject.coalesce_window ?? '';
    this.formMaxFiresPerMinute = tr.maxFiresPerMinute ? String(tr.maxFiresPerMinute) : '';
    this.formMaxConcurrentDispatches = tr.maxConcurrentDispatches
      ? String(tr.maxConcurrentDispatches)
      : '';
    this.formNotify = normalizeNotifySpec(tr.notify);
    this.formPresetId = '';
    this.formError = null;
    this.whereRowErrors = {};
    this.formAuthoringValid = true;
    this.formAuthoringErrors = [];

    const formExt = getFormAuthoring(this.formSourceKind);
    this.formAuthoringPane = formExt && tr.match.form ? 'form' : 'cel';
    if (formExt) {
      void ensureExtensionLoaded(formExt)
        .then(() => this.requestUpdate())
        .catch(() => {
          // Evicted on failure; retryable.
        });
    }
    this.dialogOpen = true;
  }

  private handleFormAuthoringOutput(out: FormAuthoringOutput): void {
    this.formAuthoringValid = out.valid !== false;
    this.formAuthoringErrors = Array.isArray(out.errors) ? [...out.errors] : [];
    // O-C: never apply types, rules, form, subject, or suggested actions from
    // an invalid form output so switching to CEL cannot save stale invalid state.
    if (!this.formAuthoringValid) {
      return;
    }
    this.formTypes = [...out.types];
    this.formRules = out.rules.map((r) => ({ ...r }));
    this.formMatchForm = out.form;
    this.formMatchFormDropped = false;
    if (out.subject) {
      this.formSubjectMode = out.subject.mode || 'new';
      this.formSubjectExpr = out.subject.expr ?? '';
      this.formCoalesceWindow = out.subject.coalesceWindow ?? '';
    }
    if (out.suggestedActions && out.suggestedActions.length > 0 && !this.editingTrigger) {
      this.formActions = out.suggestedActions.map((a) => ({ ...a }));
    }
  }

  private handleWhereRulesChanged(rules: TriggerMatchRuleSpec[]): void {
    this.formRules = rules;
    // S3 §9.2: editing any CEL row drops the form representation.
    if (this.formMatchForm !== undefined && this.formMatchForm !== null) {
      this.formMatchForm = null;
      this.formMatchFormDropped = true;
    }
  }

  private addTypeChip(): void {
    const val = this.formTypeDraft.trim();
    if (!val) return;
    if (!this.formTypes.includes(val)) {
      this.formTypes = [...this.formTypes, val];
    }
    this.formTypeDraft = '';
  }

  private removeTypeChip(typeVal: string): void {
    this.formTypes = this.formTypes.filter((t) => t !== typeVal);
  }

  private isReadOnlyActionType(type: string | undefined): boolean {
    return type !== 'message' && type !== 'dispatch_agent';
  }

  private buildDiffPatchBody(
    orig: TriggerDefinition,
    effectiveSourceId: string,
    matchPayload: Record<string, unknown>,
    subjectPayload: Record<string, unknown>
  ): Record<string, unknown> {
    const body: Record<string, unknown> = {
      expectedRevision: orig.authorizationRevision,
    };

    const nextName = this.formName.trim();
    if (nextName !== orig.name) {
      body['name'] = nextName;
    }

    const nextDesc = this.formDescription.trim();
    if (nextDesc !== (orig.description ?? '').trim()) {
      body['description'] = nextDesc;
    }

    if (this.formSourceKind !== orig.source.kind || effectiveSourceId !== orig.source.id) {
      body['source'] = {
        kind: this.formSourceKind,
        id: effectiveSourceId,
      };
    }

    const origTypes = orig.match.types ?? [];
    const origRules = orig.match.rules ?? [];
    const typesChanged = !canonicalDeepEqual(this.formTypes, origTypes);
    const rulesChanged = !canonicalDeepEqual(this.formRules, origRules);
    const formChanged =
      this.formMatchFormDropped ||
      !canonicalDeepEqual(this.formMatchForm ?? null, orig.match.form ?? null);
    if (typesChanged || rulesChanged || formChanged) {
      body['match'] = matchPayload;
    }

    const origMode = orig.subject.mode || 'new';
    const origExpr = orig.subject.expr ?? '';
    const origWindow = orig.subject.coalesce_window ?? '';
    const nextMode = this.formSubjectMode || 'new';
    const nextExpr = this.formSubjectExpr.trim();
    const nextWindow = nextMode === 'reuse' ? this.formCoalesceWindow.trim() : '';
    if (nextMode !== origMode || nextExpr !== origExpr || !durationsEqual(nextWindow, origWindow)) {
      body['subject'] = subjectPayload;
    }

    const origActions = getTriggerActions(orig);
    const actionsChanged = !canonicalDeepEqual(this.formActions, origActions, {
      stripFalsyDefaults: true,
    });
    const firstAction = this.formActions[0];
    if (actionsChanged) {
      // P9 HOOK (ptone/scion#9065): when multiActionEnabled is true, send `actions[]`,
      // preserving read-only/exec rows unchanged (O1 default-safety).
      if (this.multiActionEnabled) {
        const hasReadOnly = this.formActions.some((a) => this.isReadOnlyActionType(a.type));
        if (!hasReadOnly) {
          body['actions'] = this.formActions.map((a) => this.sanitizeActionForSave(a));
        }
      } else if (firstAction && !this.isReadOnlyActionType(firstAction.type)) {
        body['action'] = this.sanitizeActionForSave(firstAction);
      }
    }

    // O2: allow clearing limits back to 0 when the user empties the input.
    const rawFpm = this.formMaxFiresPerMinute.trim();
    const nextFpm = rawFpm === '' ? 0 : parseInt(rawFpm, 10);
    if (Number.isFinite(nextFpm) && nextFpm >= 0 && nextFpm !== (orig.maxFiresPerMinute ?? 0)) {
      body['maxFiresPerMinute'] = nextFpm;
    }

    const hasDispatchAction = this.formActions.some((a) => a.type === 'dispatch_agent');
    const rawConc = this.formMaxConcurrentDispatches.trim();
    const nextConc = rawConc === '' ? 0 : parseInt(rawConc, 10);
    if (
      Number.isFinite(nextConc) &&
      nextConc >= 0 &&
      hasDispatchAction &&
      nextConc !== (orig.maxConcurrentDispatches ?? 0)
    ) {
      body['maxConcurrentDispatches'] = nextConc;
    }

    const hasReadOnly = this.formActions.some((a) => this.isReadOnlyActionType(a.type));
    if (!hasReadOnly) {
      const nextNotify = normalizeNotifySpec(this.formNotify);
      const origNotify = normalizeNotifySpec(orig.notify);
      if (!canonicalDeepEqual(nextNotify, origNotify)) {
        body['notify'] = this.buildNotifyPayload(this.formNotify, true);
      }
    }

    return body;
  }

  /**
   * Builds a Hub-compatible TriggerNotifySpec / TriggerNotifyPatchSpec payload.
   * On PATCH, explicit empty strings for agentId/agentName clear the stored agent sink.
   */
  private buildNotifyPayload(spec: TriggerNotifySpec, forPatch: boolean): Record<string, unknown> {
    const normalized = normalizeNotifySpec(spec);
    const outcomes =
      normalized.onOutcomes && normalized.onOutcomes.length > 0
        ? [...normalized.onOutcomes]
        : [...DEFAULT_NOTIFY_OUTCOMES];
    const payload: Record<string, unknown> = {
      onOutcomes: outcomes,
      userNotice: normalized.userNotice !== false,
    };
    const agentId = (normalized.agentId ?? '').trim();
    const agentName = (normalized.agentName ?? '').trim();
    if (forPatch) {
      payload['agentId'] = agentId;
      payload['agentName'] = agentName;
    } else {
      if (agentId) payload['agentId'] = agentId;
      if (agentName) payload['agentName'] = agentName;
    }
    return payload;
  }

  /**
   * Omit untrustedVars on message and dispatch_agent payloads when saving so
   * the hub derives and stores the list from the current templates (ptone/scion#9776).
   */
  private sanitizeActionForSave(action: TriggerActionSpec): TriggerActionSpec {
    if (action.type === 'message' || action.type === 'dispatch_agent') {
      const copy = { ...action };
      delete copy.untrustedVars;
      return copy;
    }
    return action;
  }

  private async submitTriggerForm(): Promise<void> {
    if (!this.projectId) return;
    if (!this.formName.trim()) {
      this.formError = 'Name is required.';
      return;
    }
    const mode = this.sourceIdModeFor(this.formSourceKind);
    const effectiveSourceId = mode === 'none' ? '' : this.formSourceId.trim();
    if (mode === 'endpoint' && !effectiveSourceId) {
      this.formError = 'Source endpoint is required.';
      return;
    }
    const formExt = getFormAuthoring(this.formSourceKind);
    if (formExt && this.formAuthoringPane === 'form' && !this.formAuthoringValid) {
      this.formError = this.formAuthoringErrors.join('; ') || 'Fix the form errors before saving.';
      return;
    }

    const matchPayload: Record<string, unknown> = {
      types: this.formTypes,
      rules: this.formRules,
    };
    if (this.formMatchFormDropped) {
      matchPayload['form'] = null;
    } else if (this.formMatchForm !== undefined && this.formMatchForm !== null) {
      matchPayload['form'] = this.formMatchForm;
    }

    const subjectPayload: Record<string, unknown> = {
      mode: this.formSubjectMode || 'new',
    };
    if (this.formSubjectExpr.trim()) {
      subjectPayload['dialect'] = 'cel';
      subjectPayload['expr'] = this.formSubjectExpr.trim();
    }
    if (this.formSubjectMode === 'reuse' && this.formCoalesceWindow.trim()) {
      subjectPayload['coalesce_window'] = this.formCoalesceWindow.trim();
    }

    let body: Record<string, unknown>;
    if (this.editingTrigger) {
      // R1: diff against the loaded trigger so rename/description edits never
      // touch source/match/action/subject or re-attribute recorded authority.
      body = this.buildDiffPatchBody(
        this.editingTrigger,
        effectiveSourceId,
        matchPayload,
        subjectPayload
      );
    } else {
      body = {
        name: this.formName.trim(),
        description: this.formDescription.trim(),
        source: {
          kind: this.formSourceKind,
          id: effectiveSourceId,
        },
        match: matchPayload,
        subject: subjectPayload,
      };

      // P9 HOOK (ptone/scion#9065): when multiActionEnabled is true, send `actions[]`.
      // Until P9 merges on the Hub, send the single `action` object.
      const firstAction = this.formActions[0];
      if (this.multiActionEnabled) {
        body['actions'] = this.formActions
          .filter((a) => !this.isReadOnlyActionType(a.type))
          .map((a) => this.sanitizeActionForSave(a));
      } else if (firstAction && !this.isReadOnlyActionType(firstAction.type)) {
        body['action'] = this.sanitizeActionForSave(firstAction);
      }

      const maxFpm = parseInt(this.formMaxFiresPerMinute, 10);
      if (Number.isFinite(maxFpm) && maxFpm > 0) {
        body['maxFiresPerMinute'] = maxFpm;
      }
      const maxConc = parseInt(this.formMaxConcurrentDispatches, 10);
      if (
        Number.isFinite(maxConc) &&
        maxConc > 0 &&
        this.formActions.some((a) => a.type === 'dispatch_agent')
      ) {
        body['maxConcurrentDispatches'] = maxConc;
      }

      body['notify'] = this.buildNotifyPayload(this.formNotify, false);
    }

    this.saving = true;
    this.formError = null;
    this.whereRowErrors = {};
    try {
      const basePath = `/api/v1/projects/${encodeURIComponent(this.projectId)}/triggers`;
      const res = await apiFetch(
        this.editingTrigger
          ? `${basePath}/${encodeURIComponent(this.editingTrigger.id)}`
          : basePath,
        {
          method: this.editingTrigger ? 'PATCH' : 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
          suppressAccessDeniedToast: true,
        }
      );
      if (!res.ok) {
        const cloned = res.clone();
        try {
          const parsedBody = (await cloned.json()) as unknown;
          const rowErrors = parseTriggerRuleRowErrors(parsedBody);
          if (Object.keys(rowErrors).length > 0) {
            this.whereRowErrors = rowErrors;
          }
        } catch {
          // Non-JSON error body
        }
        this.formError = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      this.dialogOpen = false;
      this.editingTrigger = null;
      await this.loadTriggers();
    } catch (err) {
      this.formError = err instanceof Error ? err.message : 'Request failed';
    } finally {
      this.saving = false;
    }
  }

  private async togglePauseResume(tr: TriggerDefinition): Promise<void> {
    const action = tr.status === 'active' ? 'pause' : 'resume';
    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/triggers/${encodeURIComponent(tr.id)}/${action}`,
        { method: 'POST' }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      await this.loadTriggers();
    } catch (err) {
      this.error = err instanceof Error ? err.message : `Failed to ${action} trigger`;
    }
  }

  private async openPreview(tr: TriggerDefinition): Promise<void> {
    this.previewTrigger = tr;
    this.previewDialogOpen = true;
    this.previewLoading = true;
    this.previewError = null;
    this.previewResult = null;
    this.previewEventId = '';

    try {
      const evParams = new URLSearchParams({ limit: '1' });
      if (tr.source.id) {
        evParams.set('sourceId', tr.source.id);
      } else if (tr.source.kind) {
        evParams.set('source', tr.source.kind);
      }
      const evUrl = `/api/v1/projects/${encodeURIComponent(this.projectId)}/trigger-events?${evParams.toString()}`;
      const evRes = await apiFetch(evUrl);
      if (!evRes.ok) {
        this.previewError = await extractApiError(evRes, `HTTP ${evRes.status}`);
        return;
      }
      const evData = (await evRes.json()) as ListTriggerEventsResponse;
      const latest = evData.events?.[0];
      if (!latest) {
        this.previewError = 'No events have been received for this source yet.';
        return;
      }
      this.previewEventId = latest.id;

      const renderUrl = `/api/v1/projects/${encodeURIComponent(this.projectId)}/triggers/${encodeURIComponent(tr.id)}/render?eventId=${encodeURIComponent(latest.id)}`;
      const renderRes = await apiFetch(renderUrl, { method: 'POST' });
      if (!renderRes.ok) {
        this.previewError = await extractApiError(renderRes, `HTTP ${renderRes.status}`);
        return;
      }
      this.previewResult = (await renderRes.json()) as TriggerRenderResponse;
    } catch (err) {
      this.previewError = err instanceof Error ? err.message : 'Preview failed';
    } finally {
      this.previewLoading = false;
    }
  }

  private async confirmDelete(): Promise<void> {
    if (!this.deleteTarget) return;
    this.deleting = true;
    try {
      const target = this.deleteTarget;
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/triggers/${encodeURIComponent(target.id)}`,
        { method: 'DELETE' }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      this.deleteTarget = null;
      await this.loadTriggers();
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to delete trigger';
    } finally {
      this.deleting = false;
    }
  }

  private toggleExpand(id: string): void {
    const next = new Set(this.expandedIds);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    this.expandedIds = next;
  }

  private formatActionSummary(tr: TriggerDefinition): string {
    const actions = getTriggerActions(tr);
    if (actions.length === 0) return '—';
    if (actions.length > 1) {
      return `${actions.length} actions (${actions.map((a) => a.type).join(', ')})`;
    }
    const a = actions[0];
    if (a.type === 'exec') {
      return formatRedactedExecSummary(a);
    }
    if (a.type === 'message') {
      return `message → ${a.agentName || a.agentId || 'agent'}`;
    }
    if (a.type === 'dispatch_agent') {
      return `dispatch → ${a.template || 'template'}`;
    }
    return a.type;
  }

  private endpointLabel(source: { kind: string; id: string }): string {
    if (source.kind === 'webhook') {
      const found = this.endpoints.find((ep) => ep.id === source.id);
      if (found) return `webhook: ${found.name}`;
    }
    return `${source.kind}: ${source.id}`;
  }

  private formatNotifySummary(notify?: TriggerNotifySpec): string {
    const norm = normalizeNotifySpec(notify);
    const parts: string[] = [];
    parts.push(`User notice: ${norm.userNotice !== false ? 'on' : 'off'}`);
    if (norm.agentName || norm.agentId) {
      parts.push(`Agent: ${norm.agentName || norm.agentId}`);
    }
    parts.push(`On: ${(norm.onOutcomes ?? DEFAULT_NOTIFY_OUTCOMES).join(', ')}`);
    return parts.join(' · ');
  }

  private renderSlotPreview(label: string, slot: TriggerRenderSlotResult | undefined) {
    if (!slot) return nothing;
    return html`
      <div class="preview-slot" data-preview-slot=${label}>
        <div class="detail-label">${label}</div>
        <pre>${slot.rendered}</pre>
        ${slot.used && slot.used.length > 0
          ? html`
              <div class="var-badges">
                ${slot.used.map(
                  (u) => html`
                    <sl-badge
                      variant=${u.class === 'untrusted' ? 'warning' : 'neutral'}
                      data-var-class=${u.class}
                    >
                      ${u.path} (${u.class})
                    </sl-badge>
                  `
                )}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading trigger rules...</p>
        </div>
      `;
    }

    return html`
      <div class="toolbar">
        <sl-button
          variant="primary"
          size="small"
          data-create-trigger
          @click=${() => this.openCreateDialog()}
        >
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Rule
        </sl-button>
        <sl-button size="small" variant="default" @click=${() => this.loadTriggers()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>

      ${this.error
        ? html`
            <sl-alert variant="danger" open style="margin-bottom: 1rem;">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.error}
            </sl-alert>
          `
        : nothing}
      ${this.triggers.length === 0
        ? html`
            <div class="empty-state">
              <sl-icon name="lightning-charge"></sl-icon>
              <h3>No trigger rules</h3>
              <p>Create a rule to match incoming events and message or dispatch agents.</p>
            </div>
          `
        : html`
            <div class="table-container">
              <table>
                <thead>
                  <tr>
                    <th>Rule</th>
                    <th>Action</th>
                    <th>Status</th>
                    <th>Fires</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  ${this.triggers.map((tr) => {
                    const expanded = this.expandedIds.has(tr.id);
                    return html`
                      <tr data-trigger-row=${tr.id}>
                        <td>
                          <div style="display: flex; align-items: flex-start; gap: 0.375rem;">
                            <sl-icon-button
                              name=${expanded ? 'chevron-down' : 'chevron-right'}
                              label="Toggle rule details"
                              data-expand-trigger=${tr.id}
                              @click=${() => this.toggleExpand(tr.id)}
                            ></sl-icon-button>
                            <div>
                              <div class="rule-name">${tr.name}</div>
                              <div class="rule-meta">${this.endpointLabel(tr.source)}</div>
                            </div>
                          </div>
                        </td>
                        <td class="mono" data-trigger-action-summary=${tr.id}>
                          ${this.formatActionSummary(tr)}
                        </td>
                        <td>
                          <sl-badge variant=${tr.status === 'active' ? 'success' : 'warning'}>
                            ${tr.status}
                          </sl-badge>
                          ${tr.statusReason
                            ? html`<div class="rule-meta">${tr.statusReason}</div>`
                            : nothing}
                        </td>
                        <td>
                          <div>${tr.fireCount} fires / ${tr.errorCount} errors</div>
                          ${tr.lastFireAt
                            ? html`
                                <div
                                  class="rule-meta"
                                  title=${formatInstantWithZone(tr.lastFireAt)}
                                >
                                  Last: ${tr.lastFireStatus || '—'}
                                  (${formatRelative(tr.lastFireAt)})
                                </div>
                              `
                            : nothing}
                        </td>
                        <td class="actions-cell">
                          <sl-icon-button
                            name="eye"
                            label="Preview render"
                            data-preview-trigger=${tr.id}
                            @click=${() => this.openPreview(tr)}
                          ></sl-icon-button>
                          <sl-icon-button
                            name="pencil"
                            label="Edit rule"
                            data-edit-trigger=${tr.id}
                            @click=${() => this.openEditDialog(tr)}
                          ></sl-icon-button>
                          <sl-icon-button
                            name=${tr.status === 'active' ? 'pause-circle' : 'play-circle'}
                            label=${tr.status === 'active' ? 'Pause rule' : 'Resume rule'}
                            data-toggle-trigger=${tr.id}
                            @click=${() => this.togglePauseResume(tr)}
                          ></sl-icon-button>
                          <sl-icon-button
                            name="trash"
                            label="Delete rule"
                            data-delete-trigger=${tr.id}
                            @click=${() => {
                              this.deleteTarget = tr;
                            }}
                          ></sl-icon-button>
                        </td>
                      </tr>
                      ${expanded
                        ? html`
                            <tr class="expand-row" data-trigger-detail=${tr.id}>
                              <td colspan="5">
                                <div class="detail-grid">
                                  <div>
                                    <div class="detail-label">Match Types</div>
                                    <div class="mono">
                                      ${(tr.match.types ?? []).join(', ') || '—'}
                                    </div>
                                  </div>
                                  <div>
                                    <div class="detail-label">Where Rules</div>
                                    <div class="mono">
                                      ${tr.match.rules.length === 0
                                        ? 'Types-only (no where rules)'
                                        : tr.match.rules.map(
                                            (r, i) =>
                                              html`<div>${i + 1}. [${r.dialect}] ${r.expr}</div>`
                                          )}
                                    </div>
                                  </div>
                                  <div>
                                    <div class="detail-label">Subject &amp; Limits</div>
                                    <div class="rule-meta">
                                      Mode: ${tr.subject.mode || 'new'}
                                      ${tr.subject.coalesce_window
                                        ? ` · Coalesce: ${tr.subject.coalesce_window}`
                                        : ''}
                                      ${tr.maxFiresPerMinute
                                        ? ` · Max ${tr.maxFiresPerMinute}/min`
                                        : ''}
                                      ${tr.maxConcurrentDispatches
                                        ? ` · Max ${tr.maxConcurrentDispatches} concurrent`
                                        : ''}
                                    </div>
                                  </div>
                                  <div>
                                    <div class="detail-label">Revision &amp; Last Outcome</div>
                                    <div class="rule-meta">
                                      Rev ${tr.authorizationRevision}
                                      ${tr.lastFireStatus
                                        ? html` ·
                                            <sl-badge
                                              variant=${fireStatusBadgeVariant(tr.lastFireStatus)}
                                              >${tr.lastFireStatus}</sl-badge
                                            >`
                                        : nothing}
                                      ${tr.lastFireError
                                        ? html`<div>Error: ${tr.lastFireError}</div>`
                                        : nothing}
                                    </div>
                                  </div>
                                  <div>
                                    <div class="detail-label">Notify on Failure</div>
                                    <div class="rule-meta" data-trigger-notify-summary=${tr.id}>
                                      ${this.formatNotifySummary(tr.notify)}
                                    </div>
                                  </div>
                                </div>
                              </td>
                            </tr>
                          `
                        : nothing}
                    `;
                  })}
                </tbody>
              </table>
            </div>
          `}
      ${this.renderFormDialog()} ${this.renderPreviewDialog()} ${this.renderDeleteDialog()}
    `;
  }

  private renderFormDialog() {
    const isEdit = Boolean(this.editingTrigger);
    const formExt = getFormAuthoring(this.formSourceKind);

    return html`
      <sl-dialog
        label=${isEdit ? `Edit Rule: ${this.editingTrigger?.name}` : 'Create Trigger Rule'}
        style="--width: 44rem;"
        ?open=${this.dialogOpen}
        data-trigger-dialog
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.dialogOpen = false;
          this.editingTrigger = null;
        }}
      >
        <div class="dialog-form">
          ${this.formError
            ? html`
                <sl-alert variant="danger" open data-trigger-form-error>
                  <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                  ${this.formError}
                </sl-alert>
              `
            : nothing}

          <div class="field-grid">
            <sl-input
              label="Rule name"
              required
              placeholder="notify-on-deploy"
              .value=${this.formName}
              data-trigger-name-input
              @sl-input=${(e: Event) => {
                this.formName = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            <sl-input
              label="Description"
              placeholder="Optional description"
              .value=${this.formDescription}
              data-trigger-description-input
              @sl-input=${(e: Event) => {
                this.formDescription = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>

          <div class="field-grid">
            <sl-select
              label="Source kind"
              size="small"
              hoist
              .value=${this.formSourceKind}
              data-trigger-source-kind
              @sl-change=${(e: Event) => {
                this.handleSourceKindChange((e.target as HTMLSelectElement).value);
              }}
            >
              <sl-option value="webhook">Webhook endpoint</sl-option>
              ${getRegisteredSourceKinds().map(
                ({ sourceKind, entry }) =>
                  html`<sl-option value=${sourceKind}>${entry.label}</sl-option>`
              )}
              ${this.formSourceKind !== 'webhook' && !getSourceKind(this.formSourceKind)
                ? html`<sl-option value=${this.formSourceKind}>${this.formSourceKind}</sl-option>`
                : nothing}
            </sl-select>
            ${this.sourceIdModeFor(this.formSourceKind) === 'none'
              ? nothing
              : this.sourceIdModeFor(this.formSourceKind) === 'endpoint' &&
                  this.endpoints.length > 0
                ? html`
                    <sl-select
                      label="Webhook endpoint"
                      size="small"
                      hoist
                      .value=${this.formSourceId}
                      data-trigger-source-id
                      @sl-change=${(e: Event) => {
                        this.formSourceId = (e.target as HTMLSelectElement).value;
                      }}
                    >
                      ${this.endpoints.map(
                        (ep) => html`<sl-option value=${ep.id}>${ep.name}</sl-option>`
                      )}
                    </sl-select>
                  `
                : html`
                    <sl-input
                      label=${this.sourceIdModeFor(this.formSourceKind) === 'endpoint'
                        ? 'Webhook endpoint ID'
                        : 'Source ID'}
                      size="small"
                      placeholder="endpoint or source ID"
                      .value=${this.formSourceId}
                      data-trigger-source-id
                      @sl-input=${(e: Event) => {
                        this.formSourceId = (e.target as HTMLInputElement).value;
                      }}
                    ></sl-input>
                  `}
          </div>

          ${formExt
            ? html`
                <sl-radio-group
                  label="Authoring mode"
                  size="small"
                  .value=${this.formAuthoringPane}
                  data-authoring-mode
                  @sl-change=${(e: Event) => {
                    this.formAuthoringPane =
                      (e.target as HTMLInputElement).value === 'form' ? 'form' : 'cel';
                  }}
                >
                  <sl-radio-button value="form">${formExt.label || 'Form'}</sl-radio-button>
                  <sl-radio-button value="cel">Advanced (CEL)</sl-radio-button>
                </sl-radio-group>
              `
            : nothing}
          ${formExt && this.formAuthoringPane === 'form'
            ? html`
                <div data-form-authoring-pane>
                  ${formExt.render({
                    projectId: this.projectId,
                    sourceId: this.formSourceId,
                    ...(this.formPresetId ? { presetId: this.formPresetId } : {}),
                    initialForm: this.formMatchForm,
                    initialTypes: this.formTypes,
                    initialRules: this.formRules,
                    onChange: (out) => this.handleFormAuthoringOutput(out),
                  })}
                </div>
              `
            : html`
                <div>
                  <div class="detail-label">Event Types</div>
                  <div class="chips-row" style="margin-bottom: 0.5rem;">
                    ${this.formTypes.map(
                      (t) => html`
                        <sl-tag size="small" removable @sl-remove=${() => this.removeTypeChip(t)}>
                          ${t}
                        </sl-tag>
                      `
                    )}
                  </div>
                  <div class="chip-input-row">
                    <sl-input
                      size="small"
                      placeholder="Add event type (e.g. webhook.received)"
                      .value=${this.formTypeDraft}
                      @sl-input=${(e: Event) => {
                        this.formTypeDraft = (e.target as HTMLInputElement).value;
                      }}
                      @keydown=${(e: KeyboardEvent) => {
                        if (e.key === 'Enter') {
                          e.preventDefault();
                          this.addTypeChip();
                        }
                      }}
                    ></sl-input>
                    <sl-button size="small" variant="default" @click=${() => this.addTypeChip()}>
                      Add type
                    </sl-button>
                  </div>
                </div>

                <scion-trigger-where-editor
                  .rules=${this.formRules}
                  .admittedDialects=${ADMITTED_TRIGGER_DIALECTS}
                  .rowErrors=${this.whereRowErrors}
                  @rules-changed=${(e: CustomEvent<{ rules: TriggerMatchRuleSpec[] }>) => {
                    this.handleWhereRulesChanged(e.detail.rules);
                  }}
                ></scion-trigger-where-editor>
              `}

          <div class="section-divider">${this.multiActionEnabled ? 'Actions' : 'Action'}</div>
          ${this.formActions.some((a) => this.isReadOnlyActionType(a.type))
            ? html`
                <sl-alert variant="neutral" open data-readonly-actions-notice>
                  This rule includes read-only action types; these actions can only be edited with
                  the CLI or API. Other rule fields can still be updated here.
                </sl-alert>
              `
            : nothing}
          <scion-trigger-action-editor
            .actions=${this.formActions}
            .agents=${this.agents}
            .subjectMode=${this.formSubjectMode}
            .multiActionEnabled=${this.multiActionEnabled}
            .readonly=${this.formActions.some((a) => this.isReadOnlyActionType(a.type))}
            @actions-changed=${(e: CustomEvent<{ actions: TriggerActionSpec[] }>) => {
              this.formActions = e.detail.actions;
            }}
          ></scion-trigger-action-editor>

          <div class="section-divider">Subject &amp; Limits</div>
          <div class="field-grid">
            <sl-select
              label="Subject mode"
              size="small"
              hoist
              .value=${this.formSubjectMode}
              @sl-change=${(e: Event) => {
                this.formSubjectMode = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="new">New (always fire)</sl-option>
              <sl-option value="skip">Skip if subject active</sl-option>
              <sl-option value="reuse">Reuse live agent</sl-option>
            </sl-select>
            <sl-input
              label="Subject CEL expression (optional)"
              size="small"
              placeholder="event.subject"
              .value=${this.formSubjectExpr}
              @sl-input=${(e: Event) => {
                this.formSubjectExpr = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>

          ${this.formSubjectMode === 'reuse'
            ? html`
                <sl-input
                  label="Coalesce window (optional)"
                  size="small"
                  placeholder="30s"
                  help-text="Duration up to 10m (e.g. 30s, 2m)"
                  .value=${this.formCoalesceWindow}
                  @sl-input=${(e: Event) => {
                    this.formCoalesceWindow = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              `
            : nothing}

          <div class="field-grid">
            <sl-input
              label="Max fires per minute (optional)"
              type="number"
              size="small"
              min="0"
              max="600"
              .value=${this.formMaxFiresPerMinute}
              data-trigger-max-fpm
              @sl-input=${(e: Event) => {
                this.formMaxFiresPerMinute = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            <sl-input
              label="Max concurrent dispatches (optional)"
              type="number"
              size="small"
              min="0"
              max="50"
              .value=${this.formMaxConcurrentDispatches}
              data-trigger-max-concurrent
              @sl-input=${(e: Event) => {
                this.formMaxConcurrentDispatches = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>

          <div class="section-divider">Notify on Failure</div>
          <scion-trigger-notify-editor
            .notify=${this.formNotify}
            .agents=${this.agents}
            .readonly=${this.formActions.some((a) => this.isReadOnlyActionType(a.type))}
            @notify-changed=${(e: CustomEvent<{ notify: TriggerNotifySpec }>) => {
              this.formNotify = e.detail.notify;
            }}
          ></scion-trigger-notify-editor>
          ${formExt &&
          this.formAuthoringPane === 'form' &&
          !this.formAuthoringValid &&
          this.formAuthoringErrors.length > 0
            ? html`
                <sl-alert variant="warning" open data-form-authoring-errors>
                  <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                  ${this.formAuthoringErrors.map((msg) => html`<div>${msg}</div>`)}
                </sl-alert>
              `
            : nothing}
        </div>

        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.dialogOpen = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.saving}
          ?disabled=${this.saving ||
          (Boolean(formExt) && this.formAuthoringPane === 'form' && !this.formAuthoringValid)}
          data-submit-trigger
          @click=${() => this.submitTriggerForm()}
        >
          ${isEdit ? 'Save Rule' : 'Create Rule'}
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderPreviewDialog() {
    const res = this.previewResult;
    return html`
      <sl-dialog
        label=${`Template Preview: ${this.previewTrigger?.name ?? ''}`}
        style="--width: 44rem;"
        ?open=${this.previewDialogOpen}
        data-preview-dialog
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.previewDialogOpen = false;
          this.previewTrigger = null;
          this.previewResult = null;
        }}
      >
        ${this.previewLoading
          ? html`
              <div class="loading-state">
                <sl-spinner></sl-spinner>
                <p>Rendering against latest event...</p>
              </div>
            `
          : this.previewError
            ? html`
                <sl-alert variant="danger" open>
                  <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                  ${this.previewError}
                </sl-alert>
              `
            : res
              ? html`
                  <div class="rule-meta" style="margin-bottom: 0.75rem;">
                    Rendered against event <span class="mono">${this.previewEventId}</span>
                  </div>
                  ${res.notes?.map(
                    (n) => html`
                      <sl-alert variant="neutral" open style="margin-bottom: 0.75rem;">
                        ${n}
                      </sl-alert>
                    `
                  )}
                  ${res.errors && res.errors.length > 0
                    ? html`
                        <sl-alert variant="danger" open style="margin-bottom: 0.75rem;">
                          <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                          ${res.errors.map(
                            (err) =>
                              html`<div>
                                <strong>${err.slot}</strong> (${err.kind}):
                                ${err.message || err.path || 'Render error'}
                              </div>`
                          )}
                        </sl-alert>
                      `
                    : nothing}
                  ${this.renderSlotPreview('message', res.slots.message)}
                  ${this.renderSlotPreview('task', res.slots.task)}
                  ${this.renderSlotPreview('name', res.slots.name)}
                  ${this.renderSlotPreview('branch', res.slots.branch)}
                  ${this.renderSlotPreview('reuseMessage', res.slots.reuseMessage)}
                  ${this.renderSlotPreview('body', res.slots.body)}
                  ${res.slots.labels
                    ? Object.entries(res.slots.labels).map(([k, v]) =>
                        this.renderSlotPreview(`label:${k}`, v)
                      )
                    : nothing}
                `
              : nothing}

        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => {
            this.previewDialogOpen = false;
          }}
        >
          Close
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderDeleteDialog() {
    return html`
      <sl-dialog
        label="Delete Trigger Rule"
        ?open=${Boolean(this.deleteTarget)}
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.deleteTarget = null;
        }}
      >
        <p>Delete rule <strong>${this.deleteTarget?.name}</strong>? This cannot be undone.</p>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.deleteTarget = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="danger"
          ?loading=${this.deleting}
          data-confirm-delete-trigger
          @click=${() => this.confirmDelete()}
        >
          Delete Rule
        </sl-button>
      </sl-dialog>
    `;
  }
}

/**
 * Parses a Go-style duration string (e.g. "5m", "5m0s", "300s", "1h30m", "500ms")
 * into milliseconds for canonical equality comparison, or returns null if unparseable.
 */
export function parseDurationMs(raw: string | undefined): number | null {
  if (!raw) return null;
  const trimmed = raw.trim();
  if (!trimmed) return null;
  if (trimmed === '0') return 0;
  const unitMs: Record<string, number> = {
    ms: 1,
    s: 1000,
    m: 60_000,
    h: 3_600_000,
  };
  let total = 0;
  let pos = 0;
  const re = /(\d+(?:\.\d+)?)(ms|s|m|h)/g;
  let match: RegExpExecArray | null;
  while ((match = re.exec(trimmed)) !== null) {
    if (match.index !== pos) return null;
    const num = Number(match[1]);
    const mult = unitMs[match[2]];
    if (!Number.isFinite(num) || mult === undefined) return null;
    total += num * mult;
    pos = re.lastIndex;
  }
  return pos === trimmed.length && pos > 0 ? total : null;
}

function durationsEqual(a: string, b: string): boolean {
  const ta = a.trim();
  const tb = b.trim();
  if (ta === tb) return true;
  if (!ta || !tb) return false;
  const pa = parseDurationMs(ta);
  const pb = parseDurationMs(tb);
  if (pa !== null && pb !== null) return pa === pb;
  return false;
}

function canonicalizeValue(value: unknown, opts: { stripFalsyDefaults?: boolean } = {}): unknown {
  if (value === null || value === undefined || typeof value !== 'object') {
    return value;
  }
  if (Array.isArray(value)) {
    return value.map((item) => canonicalizeValue(item, opts));
  }
  const obj = value as Record<string, unknown>;
  const sorted: Record<string, unknown> = {};
  for (const key of Object.keys(obj).sort()) {
    const rawVal = obj[key];
    if (rawVal === undefined) continue;
    const val = canonicalizeValue(rawVal, opts);
    if (opts.stripFalsyDefaults) {
      if (val === '' || val === false) continue;
      if (Array.isArray(val) && val.length === 0) continue;
      if (
        val !== null &&
        typeof val === 'object' &&
        !Array.isArray(val) &&
        Object.keys(val).length === 0
      ) {
        continue;
      }
    }
    sorted[key] = val;
  }
  return sorted;
}

/**
 * Key-order-independent deep equality for R1 edit diffing (O-A).
 * Always normalises `undefined`/absent object properties; when
 * `stripFalsyDefaults` is set (for Go `omitempty` action structs), also
 * normalises empty strings, `false`, empty arrays, and empty objects.
 */
export function canonicalDeepEqual(
  a: unknown,
  b: unknown,
  opts: { stripFalsyDefaults?: boolean } = {}
): boolean {
  return JSON.stringify(canonicalizeValue(a, opts)) === JSON.stringify(canonicalizeValue(b, opts));
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-trigger-list': ScionTriggerList;
  }
}
