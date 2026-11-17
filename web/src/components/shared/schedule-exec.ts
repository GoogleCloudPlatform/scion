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
 * Shared validation, payload parsing, agent loading, and Lit template
 * helpers for scheduled command execution (`eventType === 'exec'`,
 * ptone/scion#7886).
 *
 * Bounds mirror `pkg/agentcontrol/exec.go` and `pkg/hub/scheduled_exec.go`
 * as a client-side convenience; the hub remains authoritative on every
 * request.
 */

import { html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { paginateAll } from '../../client/paginate-all.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';

export const MAX_EXEC_ARGC = 64;
export const MAX_EXEC_ARG_BYTES = 4096;
export const MAX_EXEC_ARGV_BYTES = 16384;
export const DEFAULT_EXEC_TIMEOUT_SEC = 60;
export const MAX_EXEC_TIMEOUT_SEC = 3600;
export const EXEC_HISTORY_LIMIT = 10;

export const EXEC_NO_OUTPUT_NOTE = "Command output isn't stored; check the agent's own logs.";

export interface ExecRunSummary {
  outcome: string;
  exitCode?: number | undefined;
  durationMs?: number | undefined;
  outputBytes?: number | undefined;
  startedAt: string;
  finishedAt?: string | undefined;
}

export interface ExecHistoryEvent {
  id: string;
  fireAt: string;
  status: string;
  firedAt?: string | undefined;
  error?: string | undefined;
  execSummary?: ExecRunSummary | undefined;
}

export interface ExecAgentOption {
  id: string;
  name: string;
  slug?: string | undefined;
  phase?: string | undefined;
  runtime?: string | undefined;
  _capabilities?: { actions?: string[] | undefined } | undefined;
}

export interface ParsedExecPayload {
  agentId: string;
  agentName: string;
  command: string[];
  timeoutSeconds: number;
  commandRedacted: boolean;
  argc: number | undefined;
  commandSha256: string | undefined;
}

const utf8Encoder = new TextEncoder();

export function utf8ByteLength(value: string): number {
  return utf8Encoder.encode(value).length;
}

/**
 * Client-side argv validation mirroring `agentcontrol.ValidateExecArgv`:
 * 1..64 arguments, non-empty `argv[0]`, no NUL bytes, <= 4096 UTF-8 bytes per
 * argument, <= 16384 UTF-8 bytes total. Empty strings in `argv[1..]` are
 * preserved as literal empty arguments.
 */
export function validateExecArgv(argv: readonly string[]): string | null {
  if (argv.length === 0) {
    return 'Command is required (add at least one argument).';
  }
  if (argv.length > MAX_EXEC_ARGC) {
    return `Command exceeds maximum argument count (${MAX_EXEC_ARGC}).`;
  }
  if (argv[0] === '') {
    return 'Executable (argv[0]) must not be empty.';
  }
  let total = 0;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg.includes('\0')) {
      return `Argument [${i}] must not contain NUL bytes.`;
    }
    const bytes = utf8ByteLength(arg);
    if (bytes > MAX_EXEC_ARG_BYTES) {
      return `Argument [${i}] exceeds maximum byte length (${MAX_EXEC_ARG_BYTES} bytes).`;
    }
    total += bytes;
  }
  if (total > MAX_EXEC_ARGV_BYTES) {
    return `Command exceeds maximum total byte length (${MAX_EXEC_ARGV_BYTES} bytes).`;
  }
  return null;
}

/**
 * Parses the timeout input field: blank maps to `0` (hub default 60s);
 * non-blank requires an integer in `1..3600`.
 */
export function parseExecTimeoutValue(raw: string): { value: number } | { error: string } {
  const trimmed = raw.trim();
  if (trimmed === '') {
    return { value: 0 };
  }
  if (!/^\d+$/.test(trimmed)) {
    return { error: `Timeout must be between 1 and ${MAX_EXEC_TIMEOUT_SEC} seconds.` };
  }
  const n = Number(trimmed);
  if (!Number.isSafeInteger(n) || n < 1 || n > MAX_EXEC_TIMEOUT_SEC) {
    return { error: `Timeout must be between 1 and ${MAX_EXEC_TIMEOUT_SEC} seconds.` };
  }
  return { value: n };
}

export function validateExecTarget(target: string): string | null {
  const trimmed = target.trim();
  if (!trimmed) {
    return 'Target agent is required.';
  }
  if (trimmed.toLowerCase() === 'all') {
    return 'Broadcast target "all" is not allowed for exec.';
  }
  return null;
}

export function argvEqual(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

/**
 * Parses a schedule or scheduled-event JSON `payload` string for an `exec`
 * row. When `commandRedacted` is true, `command` is always returned empty —
 * never reconstructed or exposed.
 */
export function parseExecPayload(raw: string): ParsedExecPayload {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return {
        agentId: '',
        agentName: '',
        command: [],
        timeoutSeconds: 0,
        commandRedacted: false,
        argc: undefined,
        commandSha256: undefined,
      };
    }
    const obj = parsed as Record<string, unknown>;
    const commandRedacted = obj.commandRedacted === true;
    const command =
      !commandRedacted && Array.isArray(obj.command)
        ? obj.command.filter((x): x is string => typeof x === 'string')
        : [];
    return {
      agentId: typeof obj.agentId === 'string' ? obj.agentId : '',
      agentName: typeof obj.agentName === 'string' ? obj.agentName : '',
      command,
      timeoutSeconds:
        typeof obj.timeoutSeconds === 'number' && Number.isFinite(obj.timeoutSeconds)
          ? obj.timeoutSeconds
          : 0,
      commandRedacted,
      argc: typeof obj.argc === 'number' ? obj.argc : undefined,
      commandSha256:
        typeof obj.commandSha256 === 'string' && obj.commandSha256 ? obj.commandSha256 : undefined,
    };
  } catch {
    return {
      agentId: '',
      agentName: '',
      command: [],
      timeoutSeconds: 0,
      commandRedacted: false,
      argc: undefined,
      commandSha256: undefined,
    };
  }
}

export function formatExecDuration(durationMs: number | undefined): string {
  if (durationMs === undefined || Number.isNaN(durationMs)) return '-';
  if (durationMs < 1000) return `${durationMs}ms`;
  const seconds = Number((durationMs / 1000).toFixed(2));
  return `${seconds}s`;
}

export function isAgentSelectableForExec(agent: ExecAgentOption): {
  selectable: boolean;
  reason?: string;
} {
  if (agent.runtime?.startsWith('managed:')) {
    return { selectable: false, reason: 'managed runtime' };
  }
  const actions = agent._capabilities?.actions;
  if (Array.isArray(actions) && !actions.includes('attach')) {
    return { selectable: false, reason: 'no attach permission' };
  }
  return { selectable: true };
}

export async function loadProjectExecAgents(
  projectId: string,
  shouldContinue: () => boolean
): Promise<ExecAgentOption[]> {
  return paginateAll<ExecAgentOption>({
    path: `/api/v1/projects/${encodeURIComponent(projectId)}/agents`,
    pageSize: 100,
    label: 'project agents',
    parsePage: (body) => {
      const data = body as { agents?: ExecAgentOption[]; nextCursor?: string };
      return { items: data.agents ?? [], nextCursor: data.nextCursor ?? '' };
    },
    shouldContinue,
  });
}

export const scheduleExecStyles = css`
  .exec-field-group {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }

  .exec-field-header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 0.5rem;
  }

  .exec-field-label {
    font-size: 0.875rem;
    font-weight: 500;
    color: var(--scion-text, #1e293b);
  }

  .exec-field-count {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    font-family: var(--scion-font-mono, monospace);
  }

  .exec-field-hint {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    line-height: 1.4;
  }

  .exec-argv-rows {
    display: flex;
    flex-direction: column;
    gap: 0.375rem;
  }

  .exec-argv-row {
    display: flex;
    align-items: center;
    gap: 0.375rem;
  }

  .exec-argv-index {
    min-width: 2rem;
    font-family: var(--scion-font-mono, monospace);
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    text-align: right;
    flex-shrink: 0;
  }

  .exec-argv-row sl-input {
    flex: 1;
  }

  .exec-argv-actions {
    display: flex;
    align-items: center;
    justify-content: flex-start;
    margin-top: 0.125rem;
  }

  .exec-argv-display {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    margin-top: 0.25rem;
    padding: 0.5rem 0.625rem;
    background: var(--scion-bg-subtle, #f1f5f9);
    border-radius: var(--scion-radius, 0.5rem);
  }

  .exec-argv-item {
    display: flex;
    align-items: baseline;
    gap: 0.5rem;
    font-family: var(--scion-font-mono, monospace);
    font-size: 0.8125rem;
    word-break: break-all;
  }

  .exec-argv-empty {
    font-style: italic;
    color: var(--scion-text-muted, #64748b);
  }

  .exec-redacted-notice {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    margin-top: 0.25rem;
    padding: 0.625rem 0.75rem;
    background: var(--scion-bg-subtle, #f1f5f9);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius, 0.5rem);
    font-size: 0.8125rem;
    color: var(--scion-text-muted, #64748b);
  }

  .exec-redacted-header {
    display: flex;
    align-items: center;
    gap: 0.375rem;
    font-weight: 500;
    color: var(--scion-text, #1e293b);
  }

  .exec-redacted-meta {
    font-family: var(--scion-font-mono, monospace);
    font-size: 0.75rem;
    word-break: break-all;
  }

  .exec-no-output-note {
    margin-top: 0.5rem;
    padding: 0.5rem 0.75rem;
    background: var(--scion-bg-subtle, #f1f5f9);
    border-radius: var(--scion-radius, 0.5rem);
    font-size: 0.8125rem;
    color: var(--scion-text-muted, #64748b);
  }

  .exec-history-section {
    margin-top: 0.75rem;
    padding-top: 0.75rem;
    border-top: 1px solid var(--scion-border, #e2e8f0);
  }

  .exec-history-title {
    font-size: 0.8125rem;
    font-weight: 600;
    color: var(--scion-text-muted, #64748b);
    margin: 0 0 0.5rem 0;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .exec-history-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.8125rem;
  }

  .exec-history-table th,
  .exec-history-table td {
    padding: 0.375rem 0.5rem;
    text-align: left;
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
  }

  .exec-history-table th {
    font-size: 0.6875rem;
    background: var(--scion-bg-subtle, #f1f5f9);
  }

  .exec-history-error {
    font-size: 0.75rem;
    color: var(--sl-color-danger-600, #dc2626);
    margin-top: 0.125rem;
  }
`;

export function renderExecAgentPicker(opts: {
  agents: readonly ExecAgentOption[];
  loading: boolean;
  loadError: string | null;
  value: string;
  currentMissingAgent?: { id: string; name: string } | undefined;
  onChange: (value: string) => void;
}): TemplateResult {
  if (opts.loadError) {
    return html`
      <sl-input
        label="Target Agent"
        class="exec-agent-fallback"
        placeholder="agent-name or agent-id"
        help-text="Agent list unavailable (${opts.loadError}); enter target agent name or ID."
        .value=${opts.value}
        @sl-input=${(e: Event): void => opts.onChange((e.target as HTMLInputElement).value)}
        required
      ></sl-input>
    `;
  }

  const hasCurrentInList = opts.agents.some((a) => a.id === opts.value);
  const missing =
    opts.currentMissingAgent && opts.currentMissingAgent.id && !hasCurrentInList
      ? opts.currentMissingAgent
      : undefined;

  const helpText = opts.loading
    ? 'Loading project agents…'
    : opts.agents.length === 0
      ? 'No agents in this project yet.'
      : 'Requires attach permission on the target agent. Stopped agents are skipped at fire time without waking.';

  return html`
    <sl-select
      label="Target Agent"
      class="exec-agent-select"
      placeholder=${opts.loading ? 'Loading agents…' : 'Select an agent'}
      help-text=${helpText}
      .value=${opts.value}
      ?disabled=${opts.loading && opts.agents.length === 0}
      @sl-change=${(e: Event): void => opts.onChange((e.target as HTMLSelectElement).value)}
    >
      ${missing
        ? html`<sl-option value=${missing.id} disabled
            >${missing.name || missing.id} (unavailable)</sl-option
          >`
        : nothing}
      ${opts.agents.map((agent) => {
        const check = isAgentSelectableForExec(agent);
        const label = agent.name || agent.slug || agent.id;
        const suffixParts: string[] = [];
        if (agent.phase) suffixParts.push(agent.phase);
        if (!check.selectable && check.reason) suffixParts.push(check.reason);
        const suffix = suffixParts.length > 0 ? ` (${suffixParts.join(', ')})` : '';
        return html`<sl-option value=${agent.id} ?disabled=${!check.selectable}
          >${label}${suffix}</sl-option
        >`;
      })}
    </sl-select>
  `;
}

export function renderExecArgvEditor(
  command: readonly string[],
  onUpdate: (updater: (prev: readonly string[]) => string[]) => void
): TemplateResult {
  const rows = command.length > 0 ? command : [''];

  const updateAt = (idx: number, val: string): void => {
    onUpdate((prev) => {
      const base = prev.length > 0 ? [...prev] : [''];
      if (idx >= 0 && idx < base.length) {
        base[idx] = val;
      }
      return base;
    });
  };

  const removeAt = (idx: number): void => {
    onUpdate((prev) => {
      if (prev.length <= 1) return [...prev];
      return prev.filter((_, i) => i !== idx);
    });
  };

  const addRow = (): void => {
    onUpdate((prev) => {
      const base = prev.length > 0 ? [...prev] : [''];
      if (base.length >= MAX_EXEC_ARGC) return base;
      return [...base, ''];
    });
  };

  return html`
    <div class="exec-field-group exec-argv-editor">
      <div class="exec-field-header">
        <span class="exec-field-label">Command (argv) *</span>
        <span class="exec-field-count">${rows.length} / ${MAX_EXEC_ARGC} args</span>
      </div>
      <div class="exec-field-hint">
        Executed directly with no shell expansion. Put the executable in [0] and one argument per
        row.
      </div>
      <div class="exec-argv-rows">
        ${rows.map(
          (arg, idx) => html`
            <div class="exec-argv-row" data-arg-row=${idx}>
              <span class="exec-argv-index">[${idx}]</span>
              <sl-input
                size="small"
                class="exec-argv-input"
                data-arg-index=${idx}
                placeholder=${idx === 0
                  ? 'Executable (e.g. /usr/bin/python3)'
                  : 'Argument (empty "" if blank)'}
                .value=${arg}
                @sl-input=${(e: Event): void => updateAt(idx, (e.target as HTMLInputElement).value)}
              ></sl-input>
              <sl-icon-button
                class="exec-argv-remove"
                name="trash"
                label="Remove argument ${idx}"
                ?disabled=${rows.length <= 1}
                @click=${(): void => removeAt(idx)}
              ></sl-icon-button>
            </div>
          `
        )}
      </div>
      <div class="exec-argv-actions">
        <sl-button
          type="button"
          size="small"
          variant="default"
          class="exec-argv-add"
          ?disabled=${rows.length >= MAX_EXEC_ARGC}
          @click=${addRow}
        >
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add argument
        </sl-button>
      </div>
    </div>
  `;
}

export function renderExecTimeoutInput(
  value: string,
  onInput: (value: string) => void
): TemplateResult {
  return html`
    <sl-input
      label="Timeout (seconds)"
      class="exec-timeout-input"
      placeholder=${String(DEFAULT_EXEC_TIMEOUT_SEC)}
      help-text="1–${MAX_EXEC_TIMEOUT_SEC} seconds (default ${DEFAULT_EXEC_TIMEOUT_SEC}s)"
      .value=${value}
      @sl-input=${(e: Event): void => onInput((e.target as HTMLInputElement).value)}
    ></sl-input>
  `;
}

export function renderExecDetailPayload(parsed: ParsedExecPayload): TemplateResult {
  const effectiveTimeout =
    parsed.timeoutSeconds > 0 ? parsed.timeoutSeconds : DEFAULT_EXEC_TIMEOUT_SEC;

  const metaParts: string[] = [];
  if (parsed.argc !== undefined) {
    metaParts.push(`${parsed.argc} ${parsed.argc === 1 ? 'arg' : 'args'}`);
  }
  if (parsed.commandSha256) {
    metaParts.push(`sha256: ${parsed.commandSha256}`);
  }

  return html`
    <div class="detail-row"><strong>Timeout:</strong> ${effectiveTimeout}s</div>
    <div class="detail-row">
      <strong>Command:</strong>
      ${parsed.commandRedacted
        ? html`
            <div class="exec-redacted-notice">
              <div class="exec-redacted-header">
                <sl-icon name="lock"></sl-icon>
                <span>Command hidden (requires attach permission on target agent)</span>
              </div>
              ${metaParts.length > 0
                ? html`<div class="exec-redacted-meta">${metaParts.join(' · ')}</div>`
                : nothing}
            </div>
          `
        : html`
            <div class="exec-argv-display">
              ${parsed.command.map(
                (arg, idx) => html`
                  <div class="exec-argv-item">
                    <span class="exec-argv-index">[${idx}]</span>
                    ${arg === ''
                      ? html`<span class="exec-argv-empty">"" (empty)</span>`
                      : html`<code>${arg}</code>`}
                  </div>
                `
              )}
            </div>
          `}
    </div>
    <div class="exec-no-output-note">${EXEC_NO_OUTPUT_NOTE}</div>
  `;
}

export function renderExecRunSummaryRows(
  summary: ExecRunSummary | undefined
): TemplateResult | typeof nothing {
  if (!summary) return nothing;
  return html`
    <div class="detail-row exec-summary-outcome">
      <strong>Outcome:</strong> <code>${summary.outcome}</code>
    </div>
    <div class="detail-row exec-summary-exit-code">
      <strong>Exit Code:</strong> ${summary.exitCode !== undefined ? summary.exitCode : '-'}
    </div>
    <div class="detail-row exec-summary-duration">
      <strong>Duration:</strong> ${formatExecDuration(summary.durationMs)}
    </div>
  `;
}

export function renderExecHistorySection(opts: {
  loading: boolean;
  error: string | null;
  events: readonly ExecHistoryEvent[];
}): TemplateResult {
  return html`
    <div class="exec-history-section">
      <div class="exec-history-title">Recent Runs</div>
      ${opts.loading
        ? html`<div class="meta-text">Loading run history…</div>`
        : opts.error
          ? html`<div class="exec-history-error">${opts.error}</div>`
          : opts.events.length === 0
            ? html`<div class="meta-text exec-history-empty">No runs recorded yet.</div>`
            : html`
                <table class="exec-history-table">
                  <thead>
                    <tr>
                      <th>Run Time</th>
                      <th>Outcome</th>
                      <th>Exit Code</th>
                      <th>Duration</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${opts.events.map((evt) => {
                      const instant = evt.firedAt ?? evt.fireAt;
                      const outcome = evt.execSummary?.outcome ?? evt.status;
                      const exitCode =
                        evt.execSummary?.exitCode !== undefined
                          ? String(evt.execSummary.exitCode)
                          : '-';
                      const duration = formatExecDuration(evt.execSummary?.durationMs);
                      return html`
                        <tr class="exec-history-row" data-event-id=${evt.id}>
                          <td>
                            <span title=${formatInstantWithZone(instant)}
                              >${formatRelative(instant)}</span
                            >
                            ${evt.error
                              ? html`<div class="exec-history-error">${evt.error}</div>`
                              : nothing}
                          </td>
                          <td><code>${outcome}</code></td>
                          <td>${exitCode}</td>
                          <td>${duration}</td>
                        </tr>
                      `;
                    })}
                  </tbody>
                </table>
              `}
    </div>
  `;
}
