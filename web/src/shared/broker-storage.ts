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
 * Display helpers for a broker's reported project storage layout and the
 * hub's layout comparison (health summary workspace_storage.layout_mismatch).
 * Every row names one published field explicitly; the descriptor is never
 * walked, so a field the broker adds later is not shown until it is named
 * here.
 */

import type { BrokerWorkspaceStorage } from './types.js';
import type { DisplayRow } from './runtime-target.js';

/** A leading URL scheme ("https://"), as the hub's registry redaction matches it. */
const SCHEME = /^[A-Za-z][A-Za-z0-9+.-]*:\/\//;

/**
 * Returns value with any userinfo (everything up to the last "@") replaced
 * by "***@", keeping a leading scheme. Mirrors the hub's display redaction
 * of image registries (config.RedactImageRegistry) without its digest
 * exception, so a value with an "@" is over-redacted, never shown as is.
 */
export function redactUserinfo(value: string): string {
  let rest = value;
  let scheme = '';
  const m = SCHEME.exec(rest);
  if (m) {
    scheme = m[0];
    rest = rest.slice(scheme.length);
  }
  const at = rest.lastIndexOf('@');
  if (at < 0) return value;
  return `${scheme}***@${rest.slice(at + 1)}`;
}

/**
 * A double-quoted value whose embedded quotes and backslashes are escaped
 * with a backslash; the match runs to the first unescaped quote.
 */
const QUOTED = /"((?:[^"\\]|\\.)*)"/g;

/** A line terminator, which "." in QUOTED does not match. */
const LINE_END = /[\n\r\u2028\u2029]/;

/**
 * Whether every quoted value in line is closed, pairing quotes as QUOTED
 * does: outside a value every quote opens one; inside a value a backslash
 * escapes the next character and an unescaped quote closes it. A backslash
 * at the end of the line or before a line terminator leaves its value
 * unclosed.
 */
function quotesBalanced(line: string): boolean {
  let inside = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (!inside) {
      if (c === '"') inside = true;
    } else if (c === '\\') {
      i++;
      if (i >= line.length || LINE_END.test(line[i])) return false;
    } else if (c === '"') {
      inside = false;
    }
  }
  return !inside;
}

/**
 * A layout comparison line with every double-quoted value redacted. When a
 * quoted value is not closed, the pairing of the other quotes is not
 * reliable, so everything after the first quote of the line is replaced
 * by "***".
 */
export function redactMismatchLine(line: string): string {
  if (!quotesBalanced(line)) return `${line.slice(0, line.indexOf('"') + 1)}***`;
  return line.replace(QUOTED, (_, v: string) => `"${redactUserinfo(v)}"`);
}

/** The hub's layout comparison lines, redacted for display; [] for none. */
export function mismatchLines(lines: readonly string[] | null | undefined): string[] {
  return (lines ?? []).filter((l) => l).map(redactMismatchLine);
}

function push(rows: DisplayRow[], label: string, value: string | undefined): void {
  if (value) rows.push({ label, value: redactUserinfo(value) });
}

/**
 * The rows of a reported storage layout: backend; the NFS share (server,
 * export, subpath root, share health) or the volume (claim, subpath root);
 * shared dirs when they are placed apart from the backend; and homes. The
 * export identity marker is not shown. [] when nothing was reported.
 */
export function storageRows(desc: BrokerWorkspaceStorage | null | undefined): DisplayRow[] {
  if (!desc || !desc.backend) return [];
  const rows: DisplayRow[] = [];
  push(rows, 'Backend', desc.backend);
  if (desc.nfs) {
    push(rows, 'NFS server', desc.nfs.server);
    push(rows, 'NFS export', desc.nfs.export);
    push(rows, 'Subpath root', desc.nfs.subPathRoot);
    rows.push({ label: 'Share health', value: desc.nfs.healthy ? 'healthy' : 'not healthy' });
  }
  if (desc.volume) {
    push(rows, 'Volume claim', desc.volume.claimName);
    push(rows, 'Subpath root', desc.volume.subPathRoot);
  }
  push(rows, 'Shared dirs', desc.sharedDirs);
  rows.push({ label: 'Homes', value: desc.homes ? redactUserinfo(desc.homes) : 'not reported' });
  return rows;
}
