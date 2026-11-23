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
 * Collapsible Advanced Palette Overrides Editor (`<scion-branding-palette-editor>`)
 * for `/admin/branding`.
 *
 * Provides structured color pickers, hex inputs, per-key/per-ramp resets,
 * env-lock handling, and per-pair WCAG 2.1 AA contrast badges for:
 * - Semantic tokens: `palette.light.*` and `palette.dark.*` (12 keys each)
 * - Shade scales: `palette.{primary,neutral,success,warning,danger}` (50–950)
 *
 * Lazy-loaded with `admin-branding.ts` (0 B entry bundle impact). Uses only
 * already-registered Shoelace components and native `<input type="color">`.
 */

import { LitElement, html, css, nothing, type PropertyValues, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { live } from 'lit/directives/live.js';

import { renderFieldErrorMessages, settingsErrorStyles } from '../shared/settings-error-views.js';
import {
  envBadgeStyles,
  readOnlyFieldStyles,
  renderEnvBadge,
  renderEnvLockBadge,
  renderKeysEnvBadge,
} from '../shared/env-badge-views.js';
import { type EnvKeyValue, envValueFor, formatEnvValue } from './admin-server-config-env-values.js';
import {
  BRANDING_KOANF_PREFIX,
  HEX_COLOR_RE,
  countPaletteOverrides,
  defaultPaletteFormState,
  isPaletteRelPathLocked,
  type PaletteFormState,
} from './admin-branding-state.js';
import {
  SCALE_NAMES,
  SCALE_STEPS,
  WCAG_AA_TEXT_MIN,
  WCAG_AA_UI_MIN,
  contrastRatio,
  formatContrastRatio,
  resolveBrandingPalette,
  type ContrastCheckResult,
  type ResolvedBrandingPalette,
  type ScaleName,
  type ScaleStep,
  type SemanticTokenKey,
} from './admin-branding-contrast.js';

const ENV_VALUE_NOT_REPORTED = 'Environment value not reported';

export interface SemanticTokenMeta {
  readonly key: SemanticTokenKey;
  readonly label: string;
  readonly pairedWith: SemanticTokenKey;
  readonly pairedLabel: string;
  readonly minRatio: number;
}

export interface SemanticTokenGroup {
  readonly id: string;
  readonly title: string;
  readonly tokens: readonly SemanticTokenMeta[];
}

export const SEMANTIC_TOKEN_GROUPS: readonly SemanticTokenGroup[] = [
  {
    id: 'surfaces',
    title: 'Surfaces & Backgrounds',
    tokens: [
      {
        key: 'bg',
        label: 'Background',
        pairedWith: 'text',
        pairedLabel: 'text',
        minRatio: WCAG_AA_TEXT_MIN,
      },
      {
        key: 'bg_subtle',
        label: 'Subtle Background',
        pairedWith: 'text',
        pairedLabel: 'text',
        minRatio: WCAG_AA_TEXT_MIN,
      },
      {
        key: 'surface',
        label: 'Surface',
        pairedWith: 'text',
        pairedLabel: 'text',
        minRatio: WCAG_AA_TEXT_MIN,
      },
      {
        key: 'surface_raised',
        label: 'Raised Surface',
        pairedWith: 'text',
        pairedLabel: 'text',
        minRatio: WCAG_AA_TEXT_MIN,
      },
    ],
  },
  {
    id: 'text',
    title: 'Text',
    tokens: [
      {
        key: 'text',
        label: 'Primary Text',
        pairedWith: 'surface',
        pairedLabel: 'surface',
        minRatio: WCAG_AA_TEXT_MIN,
      },
      {
        key: 'text_secondary',
        label: 'Secondary Text',
        pairedWith: 'surface',
        pairedLabel: 'surface',
        minRatio: WCAG_AA_TEXT_MIN,
      },
      {
        key: 'text_muted',
        label: 'Muted Text',
        pairedWith: 'surface',
        pairedLabel: 'surface',
        minRatio: WCAG_AA_TEXT_MIN,
      },
    ],
  },
  {
    id: 'controls',
    title: 'Borders & Primary Controls',
    tokens: [
      {
        key: 'border',
        label: 'Border',
        pairedWith: 'surface',
        pairedLabel: 'surface',
        minRatio: WCAG_AA_UI_MIN,
      },
      {
        key: 'primary',
        label: 'Primary',
        pairedWith: 'surface',
        pairedLabel: 'surface',
        minRatio: WCAG_AA_TEXT_MIN,
      },
      {
        key: 'primary_hover',
        label: 'Primary Hover',
        pairedWith: 'primary_text',
        pairedLabel: 'primary text',
        minRatio: WCAG_AA_TEXT_MIN,
      },
      {
        key: 'primary_active',
        label: 'Primary Active',
        pairedWith: 'primary_text',
        pairedLabel: 'primary text',
        minRatio: WCAG_AA_TEXT_MIN,
      },
      {
        key: 'primary_text',
        label: 'Primary Button Text',
        pairedWith: 'primary',
        pairedLabel: 'primary',
        minRatio: WCAG_AA_TEXT_MIN,
      },
    ],
  },
];

const SEMANTIC_TOKEN_BY_KEY = new Map<SemanticTokenKey, SemanticTokenMeta>(
  SEMANTIC_TOKEN_GROUPS.flatMap((g) => g.tokens.map((t) => [t.key, t] as const))
);

export const SCALE_LABELS: Readonly<Record<ScaleName, string>> = {
  primary: 'Primary',
  neutral: 'Neutral',
  success: 'Success',
  warning: 'Warning',
  danger: 'Danger',
};

export interface TokenContrastBadgeInfo {
  readonly ratio: number;
  readonly minRatio: number;
  readonly pass: boolean;
  readonly enforced: boolean;
  readonly pairLabel: string;
}

function pickWorstCheck(checks: readonly ContrastCheckResult[]): ContrastCheckResult {
  const failing = checks.filter((c) => !c.pass);
  const pool = failing.length > 0 ? failing : checks;
  return pool.reduce((worst, cur) => (cur.ratio < worst.ratio ? cur : worst), pool[0]);
}

/**
 * Evaluates the WCAG 2.1 AA contrast badge for a single semantic token in `mode`.
 * Always stays in agreement with `resolved.checks` (server enforcement predicates):
 * - When server checks exist for this token, reports `enforced: true` and the worst check.
 * - When no server check applies (stock untouched token or informational `border`),
 *   reports `enforced: false` with the canonical paired ratio.
 */
export function evaluateSemanticTokenBadge(
  mode: 'light' | 'dark',
  key: SemanticTokenKey,
  resolved: ResolvedBrandingPalette
): TokenContrastBadgeInfo {
  const meta = SEMANTIC_TOKEN_BY_KEY.get(key)!;
  const fullPath = `${BRANDING_KOANF_PREFIX}.palette.${mode}.${key}`;
  const modeTarget = `${mode}.${key}`;

  const matching = resolved.checks.filter((c) => {
    if (c.mode !== mode) return false;
    if (c.path === fullPath) return true;
    if (c.targetLabel === modeTarget) return true;
    if (key === 'surface' && c.id === `${mode}.ui-border`) return true;
    if (key === 'surface' && mode === 'dark' && c.id.startsWith('dark.badge-')) return true;
    return false;
  });

  if (matching.length > 0) {
    const worst = pickWorstCheck(matching);
    const allPass = matching.every((c) => c.pass);
    return {
      ratio: worst.ratio,
      minRatio: worst.minRatio,
      pass: allPass,
      enforced: true,
      pairLabel: worst.targetLabel,
    };
  }

  const tokens = mode === 'light' ? resolved.light : resolved.dark;
  const ratio = contrastRatio(tokens[key], tokens[meta.pairedWith]) ?? 1.0;
  return {
    ratio,
    minRatio: meta.minRatio,
    pass: ratio + 1e-9 >= meta.minRatio,
    enforced: false,
    pairLabel: `vs ${meta.pairedLabel}`,
  };
}

/**
 * Evaluates the WCAG 2.1 AA contrast badge for a scale step when the server
 * enforces contrast checks on that step (e.g., 600, 300, 400, 800, neutral.600/400).
 * Returns null when no contrast check applies to that step.
 */
export function evaluateScaleStepBadge(
  scale: ScaleName,
  step: ScaleStep,
  resolved: ResolvedBrandingPalette
): TokenContrastBadgeInfo | null {
  const fullPath = `${BRANDING_KOANF_PREFIX}.palette.${scale}.${step}`;
  const matching = resolved.checks.filter((c) => c.path === fullPath);
  if (matching.length === 0) return null;
  const worst = pickWorstCheck(matching);
  return {
    ratio: worst.ratio,
    minRatio: worst.minRatio,
    pass: matching.every((c) => c.pass),
    enforced: true,
    pairLabel: worst.label,
  };
}

export interface PaletteSemanticChangeDetail {
  mode: 'light' | 'dark';
  key: SemanticTokenKey;
  value: string;
}

export interface PaletteScaleChangeDetail {
  scale: ScaleName;
  step: ScaleStep;
  value: string;
}

export interface PaletteScaleClearDetail {
  scale: ScaleName;
}

export interface PaletteOpenChangeDetail {
  open: boolean;
}

@customElement('scion-branding-palette-editor')
export class ScionBrandingPaletteEditor extends LitElement {
  @property({ attribute: false })
  palette: PaletteFormState = defaultPaletteFormState();

  @property({ attribute: false })
  resolved: ResolvedBrandingPalette = resolveBrandingPalette({});

  @property({ type: Boolean })
  disabled = false;

  @property({ type: Boolean })
  open = false;

  @property({ attribute: false })
  errorScales: ReadonlySet<ScaleName> = new Set();

  @property({ attribute: false })
  serverFieldErrors: Readonly<Record<string, string[]>> = {};

  @property({ attribute: false })
  clientErrors: Readonly<Record<string, string>> = {};

  @property({ attribute: false })
  envLockedKeys: ReadonlySet<string> = new Set();

  @property({ attribute: false })
  envOverrides: readonly string[] = [];

  @property({ attribute: false })
  envValues: Readonly<Record<string, EnvKeyValue>> = {};

  @property({ type: String })
  settingsTier: 'db' | 'file' | undefined = undefined;

  @state()
  private openScales: ReadonlySet<ScaleName> = new Set();

  static override styles = css`
    ${settingsErrorStyles}
    ${envBadgeStyles}
    ${readOnlyFieldStyles}

    :host {
      display: block;
      margin-top: 1.25rem;
    }

    .advanced-details::part(base) {
      border-color: var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-bg, #f8fafc);
    }

    .advanced-details::part(header) {
      padding: 0.75rem 1rem;
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .advanced-details::part(content) {
      padding: 0 1rem 1rem 1rem;
    }

    .details-summary {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .palette-intro {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .subsection-heading {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 1.25rem 0 0.5rem 0;
      padding-bottom: 0.375rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .subsection-heading:first-of-type {
      margin-top: 0;
    }

    .role-group {
      margin-bottom: 1.25rem;
    }

    .role-group-title {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 0.625rem 0;
    }

    .semantic-rows {
      display: flex;
      flex-direction: column;
      gap: 0.625rem;
    }

    .semantic-row {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 0.75rem;
    }

    @media (max-width: 768px) {
      .semantic-row {
        grid-template-columns: 1fr;
      }
    }

    .token-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.625rem 0.75rem;
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      min-width: 0;
    }

    .token-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.375rem;
      min-height: 1.375rem;
    }

    .token-title {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      min-width: 0;
    }

    .token-title code {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .token-actions {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      flex-shrink: 0;
    }

    .color-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .color-picker-native {
      width: 2rem;
      height: 2rem;
      padding: 0.125rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-sm, 0.25rem);
      background: var(--scion-surface, #ffffff);
      cursor: pointer;
      flex-shrink: 0;
    }

    .color-picker-native:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }

    .color-row sl-input {
      flex: 1;
      min-width: 0;
    }

    sl-input::part(base) {
      font-size: 0.8125rem;
      border-color: var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    @media (pointer: coarse) {
      sl-input::part(base) {
        font-size: max(16px, 0.8125rem);
      }
    }

    sl-input::part(input) {
      color: var(--scion-text, #1e293b);
      font-family: var(--scion-font-mono, monospace);
    }

    .sr-control-label::part(form-control-label) {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    .badge-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.375rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      flex-wrap: wrap;
    }

    .badge-content {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }

    /* Non-live contrast & count pills (avoid sl-badge role="status" live-region chatter) */
    .contrast-pill,
    .count-pill {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 600;
      line-height: 1.3;
      white-space: nowrap;
      border: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-secondary, #475569);
    }

    .contrast-pill--success {
      background: var(--scion-success-bg, #dcfce7);
      color: var(--scion-success-text, #166534);
      border-color: var(--scion-success-border, #86efac);
    }

    .contrast-pill--danger,
    .count-pill--danger {
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
      border-color: var(--scion-error-border, #fca5a5);
    }

    .count-pill--primary {
      background: var(--scion-badge-primary-bg, #dbeafe);
      color: var(--scion-badge-primary-text, #1e40af);
      border-color: var(--scion-border, #e2e8f0);
    }

    .reset-btn::part(base) {
      font-size: 0.8125rem;
      padding: 0.125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .reset-btn::part(base):hover {
      color: var(--scion-text, #1e293b);
    }

    /* Shade Scales */
    .scales-list {
      display: flex;
      flex-direction: column;
      gap: 0.625rem;
    }

    .scale-details::part(base) {
      border-color: var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
    }

    .scale-details::part(header) {
      padding: 0.625rem 0.875rem;
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .scale-details::part(content) {
      padding: 0 0.875rem 0.875rem 0.875rem;
    }

    .scale-summary {
      display: flex;
      align-items: center;
      gap: 0.625rem;
      flex-wrap: wrap;
      width: 100%;
    }

    .scale-mini-strip {
      display: inline-flex;
      align-items: center;
      gap: 0.125rem;
    }

    .scale-mini-dot {
      width: 0.625rem;
      height: 0.625rem;
      border-radius: var(--scion-radius-sm, 0.25rem);
      border: 1px solid var(--scion-border, #e2e8f0);
      background: var(--dot-color, var(--scion-primary, #3b82f6));
    }

    .scale-toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      margin-bottom: 0.75rem;
      flex-wrap: wrap;
    }

    .scale-hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .scale-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(11.5rem, 1fr));
      gap: 0.5rem;
    }

    .step-card {
      background: var(--scion-bg, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.5rem;
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      min-width: 0;
    }

    .step-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.25rem;
      min-height: 1.25rem;
    }

    .step-label {
      font-size: 0.75rem;
      font-weight: 600;
      font-family: var(--scion-font-mono, monospace);
      color: var(--scion-text-secondary, #475569);
    }
  `;

  protected override willUpdate(changed: PropertyValues<this>): void {
    super.willUpdate(changed);
    if (changed.has('errorScales') && this.errorScales.size > 0) {
      const next = new Set(this.openScales);
      for (const scale of this.errorScales) {
        next.add(scale);
      }
      this.openScales = next;
    }
  }

  private isRelPathLocked(relPath: string): boolean {
    return isPaletteRelPathLocked(this.envLockedKeys, relPath);
  }

  private countPaletteErrors(): number {
    let count = 0;
    for (const key of Object.keys(this.clientErrors)) {
      if (key.startsWith('palette.')) count++;
    }
    for (const [key, msgs] of Object.entries(this.serverFieldErrors)) {
      if ((key === 'palette' || key.startsWith('palette.')) && msgs.length > 0) {
        count += msgs.length;
      }
    }
    return count;
  }

  private countScaleErrors(scale: ScaleName): number {
    const prefix = `palette.${scale}.`;
    const exact = `palette.${scale}`;
    let count = 0;
    for (const key of Object.keys(this.clientErrors)) {
      if (key === exact || key.startsWith(prefix)) count++;
    }
    for (const [key, msgs] of Object.entries(this.serverFieldErrors)) {
      if ((key === exact || key.startsWith(prefix)) && msgs.length > 0) {
        count += msgs.length;
      }
    }
    return count;
  }

  private emitSemanticChange(mode: 'light' | 'dark', key: SemanticTokenKey, value: string): void {
    this.dispatchEvent(
      new CustomEvent<PaletteSemanticChangeDetail>('palette-semantic-change', {
        detail: { mode, key, value },
        bubbles: true,
        composed: true,
      })
    );
  }

  private emitScaleChange(scale: ScaleName, step: ScaleStep, value: string): void {
    this.dispatchEvent(
      new CustomEvent<PaletteScaleChangeDetail>('palette-scale-change', {
        detail: { scale, step, value },
        bubbles: true,
        composed: true,
      })
    );
  }

  private emitScaleClear(scale: ScaleName): void {
    this.dispatchEvent(
      new CustomEvent<PaletteScaleClearDetail>('palette-scale-clear', {
        detail: { scale },
        bubbles: true,
        composed: true,
      })
    );
  }

  private emitOpenChange(open: boolean): void {
    this.dispatchEvent(
      new CustomEvent<PaletteOpenChangeDetail>('palette-open-change', {
        detail: { open },
        bubbles: true,
        composed: true,
      })
    );
  }

  private setScaleOpen(scale: ScaleName, isOpen: boolean): void {
    const next = new Set(this.openScales);
    if (isOpen) next.add(scale);
    else next.delete(scale);
    this.openScales = next;
  }

  private renderContrastBadge(
    badge: TokenContrastBadgeInfo,
    testId: string,
    isCustomized = false
  ): TemplateResult {
    const variant = !badge.enforced ? 'neutral' : badge.pass ? 'success' : 'danger';
    const statusText = !badge.enforced
      ? isCustomized
        ? 'Advisory'
        : 'Stock'
      : badge.pass
        ? 'AA Pass'
        : 'AA Fail';
    const iconName = !badge.enforced
      ? 'info-circle'
      : badge.pass
        ? 'check-circle'
        : 'exclamation-triangle';

    return html`
      <span
        class="contrast-pill contrast-pill--${variant}"
        data-token-contrast=${testId}
        data-contrast-status=${statusText.toLowerCase()}
      >
        <span class="badge-content">
          <sl-icon name=${iconName} aria-hidden="true"></sl-icon>
          ${statusText} · ${formatContrastRatio(badge.ratio)}
        </span>
      </span>
    `;
  }

  private renderEnvLockedValue(relPath: string, storedDisplay: string): TemplateResult {
    const fullKey = `${BRANDING_KOANF_PREFIX}.${relPath}`;
    const found = envValueFor(this.envValues, [fullKey]);
    const ev = found?.ev;
    return html`
      <div>${renderEnvLockBadge()} ${renderEnvBadge()}</div>
      <span class="read-only-value env-value-primary"
        >${ev ? formatEnvValue(ev) || '—' : ENV_VALUE_NOT_REPORTED}</span
      >
      ${ev?.saved_differs
        ? html`<span class="scale-hint env-value-saved"
            >${this.settingsTier === 'db' ? 'Saved value' : 'settings.yaml value'}, not in effect on
            this node: <code>${storedDisplay || '—'}</code></span
          >`
        : nothing}
    `;
  }

  private renderSemanticCell(mode: 'light' | 'dark', meta: SemanticTokenMeta): TemplateResult {
    const relPath = `palette.${mode}.${meta.key}`;
    const fullKey = `${BRANDING_KOANF_PREFIX}.${relPath}`;
    const modeLabel = mode === 'light' ? 'Light' : 'Dark';
    const controlLabel = `${modeLabel} ${meta.label}`;
    const overrideVal = this.palette[mode][meta.key];
    const resolvedHex = (mode === 'light' ? this.resolved.light : this.resolved.dark)[meta.key];
    const pickerVal = HEX_COLOR_RE.test(overrideVal.trim())
      ? overrideVal.trim().toLowerCase()
      : resolvedHex;
    const locked = this.isRelPathLocked(relPath);
    const cellDisabled = this.disabled || locked;
    const badge = evaluateSemanticTokenBadge(mode, meta.key, this.resolved);
    const clientErr = this.clientErrors[relPath];
    const serverErrs = this.serverFieldErrors[relPath] ?? [];

    return html`
      <div class="token-card" data-palette-token="${mode}.${meta.key}">
        <div class="token-header">
          <span class="token-title">
            <sl-icon name=${mode === 'light' ? 'sun' : 'moon'} aria-hidden="true"></sl-icon>
            <span>${controlLabel}</span>
            <code>${meta.key}</code>
          </span>
          <span class="token-actions">
            ${overrideVal !== '' && !cellDisabled
              ? html`
                  <sl-tooltip content="Reset ${controlLabel} to default">
                    <sl-icon-button
                      class="reset-btn"
                      name="arrow-counterclockwise"
                      label="Reset ${controlLabel} to default"
                      data-reset-token="${mode}.${meta.key}"
                      @click=${() => this.emitSemanticChange(mode, meta.key, '')}
                    ></sl-icon-button>
                  </sl-tooltip>
                `
              : nothing}
          </span>
        </div>

        ${locked
          ? this.renderEnvLockedValue(relPath, overrideVal)
          : html`
              ${renderKeysEnvBadge(this.envOverrides, this.envValues, [fullKey])}
              <div class="color-row">
                <input
                  type="color"
                  class="color-picker-native"
                  aria-label="${controlLabel} color picker"
                  data-palette-picker="${mode}.${meta.key}"
                  .value=${pickerVal}
                  ?disabled=${cellDisabled}
                  @input=${(e: Event) =>
                    this.emitSemanticChange(mode, meta.key, (e.target as HTMLInputElement).value)}
                />
                <sl-input
                  class="sr-control-label"
                  size="small"
                  label=${controlLabel}
                  data-palette-field="${mode}.${meta.key}"
                  .value=${overrideVal}
                  placeholder=${resolvedHex}
                  maxlength="7"
                  ?disabled=${cellDisabled}
                  @sl-input=${(e: Event) =>
                    this.emitSemanticChange(mode, meta.key, (e.target as HTMLInputElement).value)}
                ></sl-input>
              </div>
            `}

        <div class="badge-row">
          <span>${badge.pairLabel}</span>
          ${this.renderContrastBadge(badge, `${mode}.${meta.key}`, overrideVal.trim() !== '')}
        </div>

        ${clientErr
          ? html`<span class="field-error" data-client-error=${relPath}>${clientErr}</span>`
          : nothing}
        ${renderFieldErrorMessages(serverErrs, relPath)}
      </div>
    `;
  }

  private renderScaleSection(scale: ScaleName): TemplateResult {
    const scaleLabel = SCALE_LABELS[scale];
    const scaleForm = this.palette[scale];
    const resolvedScale = this.resolved.scales[scale];
    const overrideCount = SCALE_STEPS.filter((st) => scaleForm[st].trim() !== '').length;
    const scaleLocked = this.isRelPathLocked(`palette.${scale}`);
    const scaleErrCount = this.countScaleErrors(scale);
    const scaleLevelErrs = this.serverFieldErrors[`palette.${scale}`] ?? [];
    const isOpen = this.openScales.has(scale);

    return html`
      <sl-details
        class="scale-details"
        data-scale-details=${scale}
        ?open=${live(isOpen)}
        @sl-show=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.setScaleOpen(scale, true);
        }}
        @sl-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.setScaleOpen(scale, false);
        }}
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          if (this.openScales.has(scale)) {
            const inner = (e.currentTarget as HTMLElement).shadowRoot?.querySelector('details');
            if (inner && !inner.open) inner.open = true;
          }
        }}
      >
        <div class="scale-summary" slot="summary">
          <span>${scaleLabel} Scale (50–950)</span>
          <span class="scale-mini-strip" aria-hidden="true">
            ${SCALE_STEPS.map(
              (step) =>
                html`<span
                  class="scale-mini-dot"
                  style="--dot-color: ${resolvedScale[step]}"
                ></span>`
            )}
          </span>
          ${overrideCount > 0
            ? html`<span class="count-pill count-pill--primary" data-scale-override-count=${scale}
                >${overrideCount} ${overrideCount === 1 ? 'override' : 'overrides'}</span
              >`
            : nothing}
          ${scaleErrCount > 0
            ? html`<span class="count-pill count-pill--danger"
                >${scaleErrCount} ${scaleErrCount === 1 ? 'error' : 'errors'}</span
              >`
            : nothing}
        </div>

        <div class="scale-toolbar">
          <span class="scale-hint">
            Scale maps are replaced whole on save; cleared steps fall back to the
            ${scale === 'primary' ? 'generated or stock' : 'stock'} ramp.
          </span>
          <sl-button
            size="small"
            variant="default"
            data-clear-scale=${scale}
            ?disabled=${this.disabled || scaleLocked || overrideCount === 0}
            @click=${() => this.emitScaleClear(scale)}
          >
            <sl-icon slot="prefix" name="arrow-counterclockwise"></sl-icon>
            Clear ${scaleLabel} overrides
          </sl-button>
        </div>

        ${scaleLocked
          ? this.renderEnvLockedValue(`palette.${scale}`, `${overrideCount} steps`)
          : html`
              ${renderFieldErrorMessages(scaleLevelErrs, `palette.${scale}`)}
              <div class="scale-grid">
                ${SCALE_STEPS.map((step) => this.renderScaleStepCard(scale, step))}
              </div>
            `}
      </sl-details>
    `;
  }

  private renderScaleStepCard(scale: ScaleName, step: ScaleStep): TemplateResult {
    const scaleLabel = SCALE_LABELS[scale];
    const relPath = `palette.${scale}.${step}`;
    const controlLabel = `${scaleLabel} ${step}`;
    const overrideVal = this.palette[scale][step];
    const resolvedHex = this.resolved.scales[scale][step];
    const pickerVal = HEX_COLOR_RE.test(overrideVal.trim())
      ? overrideVal.trim().toLowerCase()
      : resolvedHex;
    const locked = this.isRelPathLocked(relPath);
    const stepDisabled = this.disabled || locked;
    const badge = evaluateScaleStepBadge(scale, step, this.resolved);
    const clientErr = this.clientErrors[relPath];
    const serverErrs = this.serverFieldErrors[relPath] ?? [];

    return html`
      <div class="step-card" data-palette-step="${scale}.${step}">
        <div class="step-header">
          <span class="step-label">${step}</span>
          ${overrideVal !== '' && !stepDisabled
            ? html`
                <sl-tooltip content="Reset ${controlLabel} to default">
                  <sl-icon-button
                    class="reset-btn"
                    name="arrow-counterclockwise"
                    label="Reset ${controlLabel} to default"
                    data-reset-step="${scale}.${step}"
                    @click=${() => this.emitScaleChange(scale, step, '')}
                  ></sl-icon-button>
                </sl-tooltip>
              `
            : nothing}
        </div>

        ${locked
          ? this.renderEnvLockedValue(relPath, overrideVal)
          : html`
              <div class="color-row">
                <input
                  type="color"
                  class="color-picker-native"
                  aria-label="${controlLabel} color picker"
                  data-scale-picker="${scale}.${step}"
                  .value=${pickerVal}
                  ?disabled=${stepDisabled}
                  @input=${(e: Event) =>
                    this.emitScaleChange(scale, step, (e.target as HTMLInputElement).value)}
                />
                <sl-input
                  class="sr-control-label"
                  size="small"
                  label=${controlLabel}
                  data-palette-scale="${scale}.${step}"
                  .value=${overrideVal}
                  placeholder=${resolvedHex}
                  maxlength="7"
                  ?disabled=${stepDisabled}
                  @sl-input=${(e: Event) =>
                    this.emitScaleChange(scale, step, (e.target as HTMLInputElement).value)}
                ></sl-input>
              </div>
            `}
        ${badge
          ? html`<div class="badge-row">
              ${this.renderContrastBadge(badge, `${scale}.${step}`)}
            </div>`
          : nothing}
        ${clientErr
          ? html`<span class="field-error" data-client-error=${relPath}>${clientErr}</span>`
          : nothing}
        ${renderFieldErrorMessages(serverErrs, relPath)}
      </div>
    `;
  }

  override render(): TemplateResult {
    const overrideCount = countPaletteOverrides(this.palette);
    const errCount = this.countPaletteErrors();
    const paletteRootErrs = this.serverFieldErrors.palette ?? [];

    return html`
      <sl-details
        class="advanced-details"
        data-advanced-palette-details
        ?open=${live(this.open)}
        @sl-show=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.emitOpenChange(true);
        }}
        @sl-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.emitOpenChange(false);
        }}
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          if (this.open) {
            const inner = (e.currentTarget as HTMLElement).shadowRoot?.querySelector('details');
            if (inner && !inner.open) inner.open = true;
          }
        }}
      >
        <span class="details-summary" slot="summary">
          <sl-icon name="sliders" aria-hidden="true"></sl-icon>
          <span>Advanced Palette Overrides</span>
          ${overrideCount > 0
            ? html`<span class="count-pill count-pill--primary" data-palette-override-count
                >${overrideCount} ${overrideCount === 1 ? 'override' : 'overrides'}</span
              >`
            : nothing}
          ${errCount > 0
            ? html`<span class="count-pill count-pill--danger" data-palette-error-count
                >${errCount} ${errCount === 1 ? 'error' : 'errors'}</span
              >`
            : nothing}
        </span>

        <p class="palette-intro">
          Fine-tune Light and Dark semantic tokens or individual 50–950 shade steps. Leave any field
          blank to inherit the derived or stock default. Contrast badges are advisory; the server
          enforces WCAG 2.1 AA on save.
        </p>

        ${renderFieldErrorMessages(paletteRootErrs, 'palette')}

        <h3 class="subsection-heading">Semantic Tokens (Light &amp; Dark)</h3>
        ${SEMANTIC_TOKEN_GROUPS.map(
          (group) => html`
            <div class="role-group" data-semantic-group=${group.id}>
              <h4 class="role-group-title">${group.title}</h4>
              <div class="semantic-rows">
                ${group.tokens.map(
                  (meta) => html`
                    <div class="semantic-row">
                      ${this.renderSemanticCell('light', meta)}
                      ${this.renderSemanticCell('dark', meta)}
                    </div>
                  `
                )}
              </div>
            </div>
          `
        )}

        <h3 class="subsection-heading">Shade Scale Overrides (50–950)</h3>
        <div class="scales-list">${SCALE_NAMES.map((scale) => this.renderScaleSection(scale))}</div>
      </sl-details>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-branding-palette-editor': ScionBrandingPaletteEditor;
  }
}
