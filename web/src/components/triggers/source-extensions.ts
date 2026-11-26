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
 * Generic source-extension registry for the Project Triggers page
 * (Stage 2 P6 / GAR G7 interface contract).
 *
 * Source providers register cards, form-authoring panes, event table columns,
 * and event detail sections keyed by `sourceKind`. Each provider owns its own
 * experiment check via `isEnabled` and may lazy-load its module via `load`.
 */

import type { TemplateResult, nothing } from 'lit';
import type {
  TriggerActionSpec,
  TriggerEventDetail,
  TriggerEventSummary,
  TriggerMatchRuleSpec,
} from './types.js';

export interface SourceCardContext {
  projectId: string;
  onNavigateToCreate?: (opts: { sourceKind: string; presetId?: string }) => void;
}

export interface SourceCardRegistration {
  /** Provider-owned experiment/feature gate (e.g. checking a source-specific flag). */
  isEnabled?: () => boolean;
  /** Optional lazy loader invoked once before rendering. */
  load?: () => Promise<unknown>;
  /** Renders the source card inside the Sources header above the tabs. */
  render: (ctx: SourceCardContext) => TemplateResult | typeof nothing;
}

export interface FormAuthoringOutput {
  types: string[];
  rules: TriggerMatchRuleSpec[];
  form: unknown;
  suggestedActions?: TriggerActionSpec[];
}

export interface FormAuthoringContext {
  projectId: string;
  sourceId: string;
  presetId?: string;
  initialForm?: unknown;
  initialTypes?: string[];
  initialRules?: TriggerMatchRuleSpec[];
  onChange: (output: FormAuthoringOutput) => void;
}

export interface FormAuthoringRegistration {
  /** Display label for the form-authoring pane (defaults to "Form"). */
  label?: string;
  /** Provider-owned experiment/feature gate. */
  isEnabled?: () => boolean;
  /** Optional lazy loader invoked once before rendering. */
  load?: () => Promise<unknown>;
  /** Renders the form-authoring pane inside the rule create/edit dialog. */
  render: (ctx: FormAuthoringContext) => TemplateResult | typeof nothing;
}

export interface EventColumnDef {
  id: string;
  header: string;
  render: (event: TriggerEventSummary) => TemplateResult | string | typeof nothing;
}

export interface EventColumnsRegistration {
  /** Provider-owned experiment/feature gate. */
  isEnabled?: () => boolean;
  /** Optional lazy loader invoked once before reading columns. */
  load?: () => Promise<unknown>;
  /** Returns extra table columns for events of `sourceKind`. */
  columns: (sourceKind: string) => EventColumnDef[];
}

export interface EventDetailRegistration {
  /** Provider-owned experiment/feature gate. */
  isEnabled?: () => boolean;
  /** Optional lazy loader invoked once before rendering. */
  load?: () => Promise<unknown>;
  /** Renders an extra detail section inside the event detail dialog. */
  render: (event: TriggerEventDetail) => TemplateResult | typeof nothing;
}

const sourceCards = new Map<string, SourceCardRegistration>();
const formAuthorings = new Map<string, FormAuthoringRegistration>();
const eventColumns = new Map<string, EventColumnsRegistration>();
const eventDetails = new Map<string, EventDetailRegistration>();
const loadedEntries = new WeakMap<object, Promise<void>>();

/**
 * Registers a source card in the Triggers page Sources header.
 * Returns an unregister function.
 */
export function registerSourceCard(sourceKind: string, entry: SourceCardRegistration): () => void {
  sourceCards.set(sourceKind, entry);
  return () => {
    if (sourceCards.get(sourceKind) === entry) {
      sourceCards.delete(sourceKind);
    }
  };
}

/**
 * Registers a form-authoring pane for a source kind in the Rules create/edit dialog.
 * Returns an unregister function.
 */
export function registerFormAuthoring(
  sourceKind: string,
  entry: FormAuthoringRegistration
): () => void {
  formAuthorings.set(sourceKind, entry);
  return () => {
    if (formAuthorings.get(sourceKind) === entry) {
      formAuthorings.delete(sourceKind);
    }
  };
}

/**
 * Registers per-source table columns for the Events and History tabs.
 * Returns an unregister function.
 */
export function registerEventColumns(
  sourceKind: string,
  entry: EventColumnsRegistration
): () => void {
  eventColumns.set(sourceKind, entry);
  return () => {
    if (eventColumns.get(sourceKind) === entry) {
      eventColumns.delete(sourceKind);
    }
  };
}

/**
 * Registers a per-source detail section for the Event detail dialog.
 * Returns an unregister function.
 */
export function registerEventDetail(
  sourceKind: string,
  entry: EventDetailRegistration
): () => void {
  eventDetails.set(sourceKind, entry);
  return () => {
    if (eventDetails.get(sourceKind) === entry) {
      eventDetails.delete(sourceKind);
    }
  };
}

function isRegistrationEnabled(entry: { isEnabled?: () => boolean }): boolean {
  return entry.isEnabled ? entry.isEnabled() : true;
}

/**
 * Lazily invokes a registration's `load()` hook at most once per registration object.
 */
export function ensureExtensionLoaded(
  entry: { load?: () => Promise<unknown> } | undefined
): Promise<void> {
  if (!entry?.load) return Promise.resolve();
  const existing = loadedEntries.get(entry);
  if (existing) return existing;
  const p = Promise.resolve(entry.load()).then(() => undefined);
  loadedEntries.set(entry, p);
  return p;
}

/** Returns all currently enabled source cards in registration order. */
export function getRegisteredSourceCards(): Array<{
  sourceKind: string;
  entry: SourceCardRegistration;
}> {
  const out: Array<{ sourceKind: string; entry: SourceCardRegistration }> = [];
  for (const [sourceKind, entry] of sourceCards.entries()) {
    if (isRegistrationEnabled(entry)) {
      out.push({ sourceKind, entry });
    }
  }
  return out;
}

/** Returns the enabled form-authoring registration for `sourceKind`, if any. */
export function getFormAuthoring(sourceKind: string): FormAuthoringRegistration | undefined {
  const entry = formAuthorings.get(sourceKind);
  if (!entry || !isRegistrationEnabled(entry)) return undefined;
  return entry;
}

/** Returns the enabled event-columns registration for `sourceKind`, if any. */
export function getEventColumnsRegistration(
  sourceKind: string
): EventColumnsRegistration | undefined {
  const entry = eventColumns.get(sourceKind);
  if (!entry || !isRegistrationEnabled(entry)) return undefined;
  return entry;
}

/** Returns extra columns for `sourceKind` from its enabled registration, or []. */
export function getEventColumns(sourceKind: string): EventColumnDef[] {
  const entry = getEventColumnsRegistration(sourceKind);
  if (!entry) return [];
  return entry.columns(sourceKind);
}

/** Returns the enabled event-detail registration for `sourceKind`, if any. */
export function getEventDetail(sourceKind: string): EventDetailRegistration | undefined {
  const entry = eventDetails.get(sourceKind);
  if (!entry || !isRegistrationEnabled(entry)) return undefined;
  return entry;
}

/**
 * @internal test-only: clears all registered source extensions.
 */
export function clearSourceExtensionsForTests(): void {
  sourceCards.clear();
  formAuthorings.clear();
  eventColumns.clear();
  eventDetails.clear();
}
