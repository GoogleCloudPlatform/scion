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
 * Static regression guard for forced-attachment downloads (ptone/scion#9913).
 *
 * The HTML `download` attribute is ignored on cross-origin object-storage
 * redirects, so every download affordance pointing at an artifact file or
 * chat attachment must use a forced-attachment URL (`artifactDownloadUrl`,
 * `buildAttachmentDownloadUrl`, `withDownloadParam`, or `?download=1`).
 */

import { describe, expect, it } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, resolve, sep } from 'node:path';

const SRC_ROOT = resolve(__dirname, '..');

function walkTsFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name === '__fixtures__' || entry.name === 'node_modules') continue;
      out.push(...walkTsFiles(full));
    } else if (
      entry.isFile() &&
      entry.name.endsWith('.ts') &&
      !entry.name.endsWith('.test.ts') &&
      !entry.name.endsWith('.spec.ts') &&
      !entry.name.endsWith('.d.ts')
    ) {
      out.push(full);
    }
  }
  return out;
}

/**
 * Extracts opening tags for `<a ...>`, `<sl-button ...>`, and
 * `<sl-icon-button ...>` while respecting `${ ... }` template interpolations
 * so `=>` inside an event handler does not end the tag prematurely.
 */
function extractOpeningTags(source: string): Array<{ tag: string; line: number }> {
  const tags: Array<{ tag: string; line: number }> = [];
  const startRe = /<(?:a|sl-button|sl-icon-button)(?=[\s/>])/g;
  let match: RegExpExecArray | null;
  while ((match = startRe.exec(source)) !== null) {
    const start = match.index;
    let i = start + match[0].length;
    let interpDepth = 0;
    let quote: string | null = null;
    while (i < source.length) {
      const ch = source[i];
      if (quote) {
        if (ch === '\\') {
          i += 2;
          continue;
        }
        if (ch === quote) quote = null;
        i++;
        continue;
      }
      if (interpDepth === 0 && (ch === '"' || ch === "'")) {
        quote = ch;
        i++;
        continue;
      }
      if (ch === '$' && source[i + 1] === '{') {
        interpDepth++;
        i += 2;
        continue;
      }
      if (interpDepth > 0) {
        if (ch === '{') interpDepth++;
        else if (ch === '}') interpDepth--;
        i++;
        continue;
      }
      if (ch === '>') {
        const line = source.slice(0, start).split('\n').length;
        tags.push({ tag: source.slice(start, i + 1), line });
        break;
      }
      i++;
    }
  }
  return tags;
}

/**
 * Returns true if an `href` value uses an unforced artifact or attachment URL
 * (i.e. raw `artifactFileUrl`, `attachmentURL`, `buildAttachmentApiUrl`, or a
 * raw `/api/v1/chat/attachments/` or `/api/v1/artifacts/.../files/` path
 * without `withDownloadParam` / `artifactDownloadUrl` / `buildAttachmentDownloadUrl`
 * / `download=1`).
 */
function isUnforcedArtifactOrAttachmentHref(hrefExpr: string): boolean {
  if (
    /\b(?:artifactDownloadUrl|buildAttachmentDownloadUrl|withDownloadParam)\s*\(/.test(hrefExpr) ||
    /[?&]download=(?:1|true)\b/.test(hrefExpr)
  ) {
    return false;
  }
  return (
    /\bartifactFileUrl\s*\(/.test(hrefExpr) ||
    /\battachmentURL\s*\(/.test(hrefExpr) ||
    /\bbuildAttachmentApiUrl\s*\(/.test(hrefExpr) ||
    /\/api\/v1\/chat\/attachments\//.test(hrefExpr) ||
    /\/api\/v1\/artifacts\/[^'"`\s]*\/files\//.test(hrefExpr)
  );
}

/**
 * Extracts the value of the `href=...` attribute from a single opening tag.
 */
function extractHrefValue(tag: string): string | null {
  const m = /\bhref\s*=\s*/.exec(tag);
  if (!m) return null;
  const start = m.index + m[0].length;
  const ch = tag[start];
  if (ch === '"' || ch === "'") {
    const end = tag.indexOf(ch, start + 1);
    return end >= 0 ? tag.slice(start + 1, end) : null;
  }
  if (ch === '$' && tag[start + 1] === '{') {
    let depth = 1;
    let i = start + 2;
    while (i < tag.length && depth > 0) {
      if (tag[i] === '{') depth++;
      else if (tag[i] === '}') depth--;
      i++;
    }
    return tag.slice(start, i);
  }
  const bare = /^[^\s/>]+/.exec(tag.slice(start));
  return bare ? bare[0] : null;
}

export function findForcedDownloadViolations(source: string, relPath: string): string[] {
  const violations: string[] = [];

  for (const { tag, line } of extractOpeningTags(source)) {
    const isDownloadAffordance =
      /\bdownload(?:\s*=|[\s/>])/.test(tag) ||
      /\bname\s*=\s*["']download["']/.test(tag) ||
      /\bdownload-chip\b/.test(tag);
    if (!isDownloadAffordance) continue;

    const href = extractHrefValue(tag);
    if (!href) continue;

    if (isUnforcedArtifactOrAttachmentHref(href)) {
      violations.push(
        `${relPath}:${line}: download affordance links to an unforced URL (${href.trim()}) — use artifactDownloadUrl(), buildAttachmentDownloadUrl(), or withDownloadParam()`
      );
    }
  }

  // Catch programmatic `<a download>` patterns where `.href = ...` is paired
  // with `.download = ...` in a nearby line using an unforced URL.
  const lines = source.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const hrefMatch = /\.\s*href\s*=\s*([^;]+)/.exec(lines[i]);
    if (!hrefMatch) continue;
    const windowText = lines.slice(Math.max(0, i - 3), Math.min(lines.length, i + 4)).join('\n');
    if (!/\.\s*download\s*=/.test(windowText)) continue;
    if (isUnforcedArtifactOrAttachmentHref(hrefMatch[1])) {
      violations.push(
        `${relPath}:${i + 1}: programmatic download assigns unforced URL (${hrefMatch[1].trim()}) — use artifactDownloadUrl(), buildAttachmentDownloadUrl(), or withDownloadParam()`
      );
    }
  }

  return violations;
}

describe('forced-attachment download regression guard (ptone/scion#9913)', () => {
  it('verifies every download affordance in web/src uses a forced-attachment URL', () => {
    const violations: string[] = [];
    for (const file of walkTsFiles(SRC_ROOT)) {
      const rel = relative(SRC_ROOT, file).split(sep).join('/');
      const source = readFileSync(file, 'utf8');
      violations.push(...findForcedDownloadViolations(source, rel));
    }
    expect(violations).toEqual([]);
  });

  it('catches unforced download links in templates and programmatic anchors', () => {
    const badTemplate = `
      html\`
        <sl-icon-button name="download" href=\${attachmentURL(ref.id)} download=\${ref.name}></sl-icon-button>
        <a class="download-chip" href="/api/v1/chat/attachments/\${file.id}" download=\${file.name}></a>
        <sl-button href=\${artifactFileUrl(id, seq, path)} download=\${name}>Download</sl-button>
        <sl-button href=\${buildAttachmentApiUrl(id)} download=\${name}>Download</sl-button>
      \`;
    `;
    expect(findForcedDownloadViolations(badTemplate, 'fixture.ts')).toHaveLength(4);

    const badProgrammatic = `
      const a = document.createElement('a');
      a.href = attachmentURL(file.id);
      a.download = file.name;
      a.click();
    `;
    expect(findForcedDownloadViolations(badProgrammatic, 'fixture.ts')).toHaveLength(1);
  });

  it('allows forced-attachment downloads and non-download inline/raw links', () => {
    const goodSource = `
      html\`
        <sl-icon-button name="download" href=\${buildAttachmentDownloadUrl(ref.id)} download=\${ref.name}></sl-icon-button>
        <a class="download-chip" href=\${buildAttachmentDownloadUrl(file.id)} download=\${file.name}></a>
        <sl-button href=\${artifactDownloadUrl(id, seq, path)} download=\${name}>Download</sl-button>
        <sl-button href=\${artifactFileUrl(id, seq, path)} target="_blank" rel="noopener noreferrer">Open raw</sl-button>
        <img src=\${attachmentURL(img.id)} alt=\${img.name} />
      \`;
      triggerFileDownload(buildAttachmentDownloadUrl(file.id), file.name);
    `;
    expect(findForcedDownloadViolations(goodSource, 'fixture.ts')).toEqual([]);
  });
});
