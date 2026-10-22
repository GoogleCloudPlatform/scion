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
 * Classifies the error bodies the hub returns from admin settings saves, so
 * the settings pages can show them next to the field or section they concern
 * (ptone/scion#3923).
 *
 * The hub sends two envelopes:
 *
 * - flat: `{error: "<code>", message?, errors?, keys?, conflicted?,
 *   applied_keys?, pending_restart?, skipped?}`, used by
 *   the server-config PUT for `validation_failed` (400, `errors` maps a
 *   settings section to `[{Path, Message}]`, where Path is a JSON pointer
 *   inside that section's document), `revision_conflict` (409) and
 *   `layer0_rejected` / `*_keys_rejected` (422, `keys` lists koanf paths);
 * - nested: `{error: {code, message, details?}}`, from the Go writeError
 *   helper, used for `validation_error` (422) and everything else.
 *
 * Field keys are koanf paths (for example `server.hub.public_url`). Entry
 * keys are accepted capitalized (`Path`, `Message`, as the hub encodes them
 * today) or lowercase (`path`, `field`, `message`). Field names are never
 * parsed out of message text: an error without a structured field is shown
 * as a section-level summary.
 */

export type SettingsSaveErrorKind = 'validation' | 'conflict' | 'layer0' | 'generic';

export interface SettingsFieldError {
  /** Settings section the server named (opsettings section), if any. */
  section: string | null;
  /** koanf path of the field, or null when it maps to no single field. */
  key: string | null;
  /** The path as the server sent it (JSON pointer or dotted), may be empty. */
  path: string;
  message: string;
}

export interface SettingsConflict {
  section: string;
  expected_revision?: number | undefined;
  current_revision?: number | undefined;
}

/** A key saved but not in use, with the reason (409 `skipped`). */
export interface SettingsSkippedKey {
  key: string;
  reason: string;
}

export interface SettingsSaveError {
  kind: SettingsSaveErrorKind;
  status: number;
  /** Server error code (flat `error` string or nested `error.code`), or ''. */
  code: string;
  /** Server message text, or null when the body carries none. */
  message: string | null;
  /** Validation entries (kind 'validation'). */
  fieldErrors: SettingsFieldError[];
  /** koanf keys named by a `*_rejected` body (layer0 and similar). */
  keys: string[];
  /** Conflicted sections (kind 'conflict'). */
  conflicted: SettingsConflict[];
  /**
   * Keys of the sections written before the conflict (kind 'conflict'),
   * from the 409 body (ptone/scion#4467): applied to the running hub now,
   * saved but taking effect only after a restart, and saved but not in use.
   */
  appliedKeys: string[];
  pendingRestart: string[];
  skipped: SettingsSkippedKey[];
}

export interface SettingsErrorContext {
  /**
   * Settings section name to its koanf paths, as served by
   * GET /api/v1/admin/server-config/schema (see sectionPathsFromSchema).
   * Used to turn a section-relative JSON pointer into a koanf key. There is
   * no built-in copy of the registry, so it cannot drift: without this
   * mapping a section-relative path maps to no field and the entry is shown
   * in the section summary.
   */
  sectionKoanfPaths?: Record<string, readonly string[]> | undefined;
  /** Section to assume for entries that name no section. */
  defaultSection?: string | undefined;
}

const VALIDATION_CODES = new Set(['validation_failed', 'validation_error']);

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * The section -> koanf paths mapping from a parsed
 * GET /api/v1/admin/server-config/schema body
 * (`{sections: {<name>: {koanf_paths: [...]}}}`), or undefined when the body
 * has none.
 */
export function sectionPathsFromSchema(body: unknown): Record<string, string[]> | undefined {
  if (!isRecord(body) || !isRecord(body.sections)) return undefined;
  const out: Record<string, string[]> = {};
  for (const [name, sec] of Object.entries(body.sections)) {
    const paths = isRecord(sec) && Array.isArray(sec.koanf_paths) ? sec.koanf_paths : [];
    const strings = paths.filter((k): k is string => typeof k === 'string' && k !== '');
    if (strings.length > 0) out[name] = strings;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** The non-empty strings of v when it is an array, else []. */
function stringList(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((k): k is string => typeof k === 'string' && k !== '') : [];
}

function str(v: unknown): string | null {
  return typeof v === 'string' && v !== '' ? v : null;
}

/**
 * Splits a JSON pointer ("a/b/0"), dotted ("a.b") or indexed ("a[0].b") path.
 * Pointer escapes (~1 for "/", ~0 for "~") are decoded per segment. A "." is
 * always a separator, so a map key that contains one (a profile or harness
 * config name) is split; the key then still falls under the section prefix,
 * only its inline placement is lost.
 */
function pathSegments(path: string): string[] {
  return path
    .replace(/\[(\d+)\]/g, '.$1')
    .split(/[/.]/)
    .filter((s) => s !== '')
    .map((s) => s.replace(/~1/g, '/').replace(/~0/g, '~'));
}

/**
 * The key for a path the section mapping could not resolve: the path's
 * segments under `prefix` (for example `server.github_app` + `private_key`,
 * or `settings` + `/bot_token`). Returns null for an empty path.
 */
export function fallbackFieldKey(prefix: string | null, path: string): string | null {
  const segs = pathSegments(path);
  if (segs.length === 0) return null;
  return [...(prefix ? [prefix] : []), ...segs].join('.');
}

/**
 * Maps a section-relative path to a koanf key using the section's koanf
 * paths. Returns null for an empty path (the section as a whole) or a path
 * that maps to no known key.
 */
export function resolveFieldKey(
  section: string | null,
  path: string,
  ctx: SettingsErrorContext = {}
): string | null {
  const segs = pathSegments(path);
  if (segs.length === 0) return null;
  const dotted = segs.join('.');
  const paths = section ? ctx.sectionKoanfPaths?.[section] : undefined;
  if (!paths || paths.length === 0) {
    // No section to resolve against: a path that is already a koanf key
    // (dotted, as in a `keys` list) is used as is.
    return section ? null : dotted;
  }
  // Longest prefix of the path that names a koanf path of the section; the
  // remainder (array index, nested member) stays on the key so a field
  // renderer can match it by prefix.
  for (let n = segs.length; n > 0; n--) {
    const prefix = segs.slice(0, n).join('.');
    const rest = segs.slice(n);
    // Among the matches prefer the one with the fewest segments, so the
    // result does not depend on registry order (telemetry "enabled" is
    // telemetry.enabled, not telemetry.cloud.enabled).
    const match = paths
      .filter((p) => p === prefix || p.endsWith(`.${prefix}`))
      .sort((x, y) => x.split('.').length - y.split('.').length)[0];
    if (match) return rest.length > 0 ? `${match}.${rest.join('.')}` : match;
  }
  // Map-valued sections (runtimes, profiles, harness_configs) register the
  // section prefix only; nested paths hang below it.
  if (paths.length === 1 && paths[0] === section) return `${section}.${dotted}`;
  return null;
}

function entryFromUnknown(
  raw: unknown,
  section: string | null,
  ctx: SettingsErrorContext
): SettingsFieldError | null {
  if (typeof raw === 'string') {
    return raw ? { section, key: null, path: '', message: raw } : null;
  }
  if (!isRecord(raw)) return null;
  const message = str(raw.Message) ?? str(raw.message);
  if (!message) return null;
  const path = str(raw.Path) ?? str(raw.path) ?? str(raw.field) ?? str(raw.key) ?? '';
  return { section, key: resolveFieldKey(section, path, ctx), path, message };
}

/** Reads a section->entries map or an entries list into field errors. */
function collectEntries(errors: unknown, ctx: SettingsErrorContext): SettingsFieldError[] | null {
  const out: SettingsFieldError[] = [];
  const defaultSection = ctx.defaultSection ?? null;
  if (Array.isArray(errors)) {
    for (const raw of errors) {
      const e = entryFromUnknown(raw, defaultSection, ctx);
      if (e) out.push(e);
    }
    return out;
  }
  if (isRecord(errors)) {
    for (const [section, list] of Object.entries(errors)) {
      const items = Array.isArray(list) ? list : [list];
      for (const raw of items) {
        const e = entryFromUnknown(raw, section, ctx);
        if (e) out.push(e);
      }
    }
    return out;
  }
  return null;
}

function nestedValidationEntries(
  message: string | null,
  details: Record<string, unknown> | null,
  ctx: SettingsErrorContext
): SettingsFieldError[] {
  const section = str(details?.section) ?? ctx.defaultSection ?? null;
  const field = str(details?.field) ?? str(details?.path) ?? str(details?.key);
  if (field && message) {
    return [{ section, key: resolveFieldKey(section, field, ctx), path: field, message }];
  }
  const out: SettingsFieldError[] = [];
  if (message) out.push({ section, key: null, path: '', message });
  const listed = details
    ? collectEntries(details.errors, { ...ctx, defaultSection: section ?? undefined })
    : null;
  if (listed) out.push(...listed);
  return out;
}

/**
 * Classifies a failed settings save. `body` is the parsed JSON body, or
 * undefined when it was not JSON.
 */
export function classifySettingsError(
  status: number,
  body: unknown,
  ctx: SettingsErrorContext = {}
): SettingsSaveError {
  const result: SettingsSaveError = {
    kind: 'generic',
    status,
    code: '',
    message: null,
    fieldErrors: [],
    keys: [],
    conflicted: [],
    appliedKeys: [],
    pendingRestart: [],
    skipped: [],
  };
  if (!isRecord(body)) return result;

  const nested = isRecord(body.error) ? body.error : null;
  result.code = str(body.error) ?? str(nested?.code) ?? '';
  result.message = str(body.message) ?? str(nested?.message);
  const details = isRecord(nested?.details) ? nested.details : null;

  const keys = Array.isArray(body.keys)
    ? body.keys
    : Array.isArray(details?.keys)
      ? details.keys
      : [];
  result.keys = keys.filter((k): k is string => typeof k === 'string' && k !== '');

  if (VALIDATION_CODES.has(result.code)) {
    const entries = nested
      ? nestedValidationEntries(result.message, details, ctx)
      : (collectEntries(body.errors, ctx) ?? []);
    if (entries.length === 0 && result.message) {
      entries.push({
        section: ctx.defaultSection ?? null,
        key: null,
        path: '',
        message: result.message,
      });
    }
    if (entries.length > 0) {
      result.kind = 'validation';
      result.fieldErrors = entries;
    }
    return result;
  }

  if (result.code === 'revision_conflict') {
    result.kind = 'conflict';
    const list = Array.isArray(body.conflicted) ? body.conflicted : [];
    result.conflicted = list
      .filter(isRecord)
      .filter((c) => typeof c.section === 'string')
      .map((c) => ({
        section: c.section as string,
        expected_revision:
          typeof c.expected_revision === 'number' ? c.expected_revision : undefined,
        current_revision: typeof c.current_revision === 'number' ? c.current_revision : undefined,
      }));
    // The sections written before the conflict stay saved; their keys are
    // reported like a successful save's (ptone/scion#4467).
    result.appliedKeys = stringList(body.applied_keys);
    result.pendingRestart = stringList(body.pending_restart);
    result.skipped = (Array.isArray(body.skipped) ? body.skipped : [])
      .filter(isRecord)
      .filter((k) => typeof k.key === 'string' && k.key !== '')
      .map((k) => ({ key: k.key as string, reason: typeof k.reason === 'string' ? k.reason : '' }));
    return result;
  }

  if (result.code === 'layer0_rejected') {
    result.kind = 'layer0';
    return result;
  }

  return result;
}

/** Reads and classifies a failed settings save response. */
export async function readSettingsSaveError(
  res: Response,
  ctx: SettingsErrorContext = {}
): Promise<SettingsSaveError> {
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    body = undefined;
  }
  return classifySettingsError(res.status, body, ctx);
}

/**
 * Messages for the field rendered under `koanfKey`: entries whose key is the
 * field's key or a path below it (an array element or nested member).
 */
export function fieldErrorMessages(
  errors: readonly SettingsFieldError[] | null | undefined,
  koanfKey: string
): string[] {
  if (!errors) return [];
  return errors
    .filter((e) => e.key !== null && (e.key === koanfKey || e.key.startsWith(`${koanfKey}.`)))
    .map((e) => e.message);
}

/**
 * The generic message for a save failure: the page's fallback text, followed
 * by the server's message when it sent one.
 */
export function genericSaveErrorText(err: SettingsSaveError, fallback: string): string {
  return err.message ? `${fallback}: ${err.message}` : fallback;
}
