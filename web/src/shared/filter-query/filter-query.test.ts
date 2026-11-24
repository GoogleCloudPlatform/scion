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

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import {
  VIEW_SCHEMAS,
  canonical,
  getActiveSingleKeyValue,
  getSuggestions,
  getViewSchema,
  knownKeysFor,
  match,
  parse,
  serverSubset,
  setSingleKeyInQuery,
  validate,
  type FilterRow,
  type Query,
  type Term,
  type ViewSchema,
} from './index.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, '..', '..', '..', '..');
const CONFORMANCE_PATH = resolve(REPO_ROOT, 'pkg/filterquery/testdata/conformance.json');
const DOCS_PATH = resolve(REPO_ROOT, 'docs-site/src/content/docs/reference/filter-queries.md');

interface ConformanceFile {
  parse: Array<{
    name: string;
    input: string;
    knownKeys: string[];
    expect?: {
      mode: 'text' | 'structured';
      text?: string;
      terms?: Term[];
      excludeText?: string[];
    };
    canonical?: string;
    error?: {
      code: string;
      offset: number;
      length: number;
      utf16Offset?: number;
      utf16Length?: number;
    };
  }>;
  eval: {
    knownKeys: string[];
    rows: Array<{
      id: string;
      name: string;
      slug: string;
      phase: string;
      labels: Record<string, string>;
    }>;
    cases: Array<{
      name: string;
      query: string;
      expectIds: string[];
    }>;
  };
}

const conformance = JSON.parse(readFileSync(CONFORMANCE_PATH, 'utf8')) as ConformanceFile;

function makeKnown(keys: string[]): (k: string) => boolean {
  const set = new Set(keys.map((k) => k.toLowerCase()));
  return (k: string) => set.has(k.toLowerCase());
}

describe('filter-query conformance (parse)', () => {
  for (const tc of conformance.parse) {
    it(tc.name, () => {
      const known = makeKnown(tc.knownKeys);
      const res = parse(tc.input, known);
      if (tc.error) {
        expect(res.error).toBeDefined();
        expect(res.error?.code).toBe(tc.error.code);
        expect(res.error?.offset).toBe(tc.error.utf16Offset ?? tc.error.offset);
        expect(res.error?.length).toBe(tc.error.utf16Length ?? tc.error.length);
        return;
      }
      expect(res.error).toBeUndefined();
      expect(res.query).toBeDefined();
      const q = res.query as Query;
      expect(q.mode).toBe(tc.expect?.mode);
      if (q.mode === 'text') {
        expect(q.text).toBe(tc.expect?.text ?? '');
      } else {
        expect(q.text ?? '').toBe(tc.expect?.text ?? '');
        expect(q.terms).toEqual(tc.expect?.terms ?? []);
        expect(q.excludeText).toEqual(tc.expect?.excludeText);
      }
      if (tc.canonical !== undefined) {
        const gotCanon = canonical(q);
        expect(gotCanon).toBe(tc.canonical);
        const reparsed = parse(gotCanon, known);
        expect(reparsed.error).toBeUndefined();
        expect(canonical(reparsed.query as Query)).toBe(gotCanon);
      }
    });
  }
});

describe('filter-query conformance (eval)', () => {
  const known = makeKnown(conformance.eval.knownKeys);
  const rows: Array<{ id: string; row: FilterRow }> = conformance.eval.rows.map((r) => ({
    id: r.id,
    row: {
      values(key: string): string[] {
        if (key === 'phase') return r.phase ? [r.phase] : [];
        if (key === 'label') {
          const out: string[] = [];
          for (const [k, v] of Object.entries(r.labels)) {
            out.push(k, `${k}=${v}`);
          }
          return out;
        }
        return [];
      },
      text(): string[] {
        return [r.name, r.slug];
      },
    },
  }));

  for (const tc of conformance.eval.cases) {
    it(tc.name, () => {
      const res = parse(tc.query, known);
      expect(res.error).toBeUndefined();
      const gotIds = rows.filter((r) => match(res.query as Query, r.row)).map((r) => r.id);
      expect(gotIds).toEqual(tc.expectIds);
    });
  }
});

describe('validate, warnings, serverSubset, suggest, and query-edit', () => {
  const agentsSchema = getViewSchema('agents') as ViewSchema;

  it('validates enum values with closest-match hint', () => {
    const res = parse('phase:runing', knownKeysFor(agentsSchema));
    expect(res.error).toBeUndefined();
    const v = validate(res.query as Query, agentsSchema, 'phase:runing');
    expect(v.error?.code).toBe('value');
    expect(v.error?.message).toContain('running');
  });

  it('warns on unknown key and repeated single-valued key without blocking', () => {
    const raw = 'phase:running phase:error foo:bar';
    const res = parse(raw, knownKeysFor(agentsSchema));
    expect(res.error).toBeUndefined();
    const v = validate(res.query as Query, agentsSchema, raw);
    expect(v.error).toBeUndefined();
    expect(v.warnings.some((w) => w.code === 'unknown_key')).toBe(true);
    expect(v.warnings.some((w) => w.code === 'repeated_key')).toBe(true);
  });

  it('does not emit unknown-key warning for URL schemes like https://', () => {
    const raw = 'https://example.com/x';
    const res = parse(raw, knownKeysFor(agentsSchema));
    expect(res.error).toBeUndefined();
    const v = validate(res.query as Query, agentsSchema, raw);
    expect(v.error).toBeUndefined();
    expect(v.warnings).toEqual([]);
  });

  it('strips client-only keys in serverSubset', () => {
    const schemaWithClient: ViewSchema = {
      ...agentsSchema,
      keys: [
        ...agentsSchema.keys,
        {
          name: 'mode',
          help: 'Message mode',
          kind: 'enum',
          exec: 'client',
          enum: ['lineage', 'branch'],
          multiValued: false,
        },
      ],
    };
    const res = parse('phase:running mode:lineage api', knownKeysFor(schemaWithClient));
    expect(res.error).toBeUndefined();
    expect(serverSubset(res.query as Query, schemaWithClient)).toBe('phase:running api');

    // When all key terms are stripped, key-shaped text stays quoted so the hub
    // does not re-parse it as a structured key term.
    const quotedTextOnly = parse('mode:lineage "phase:error"', knownKeysFor(schemaWithClient));
    expect(quotedTextOnly.error).toBeUndefined();
    expect(serverSubset(quotedTextOnly.query as Query, schemaWithClient)).toBe('"phase:error"');
  });

  it('suggests keys at boundary and enum values after key:', () => {
    const keySugs = getSuggestions('ph', 2, agentsSchema);
    expect(keySugs.map((s) => s.label)).toContain('phase:');

    const valSugs = getSuggestions('phase:ru', 8, agentsSchema);
    expect(valSugs.map((s) => s.label)).toContain('running');
  });

  it('quotes dynamic value suggestions containing commas, quotes, or spaces', () => {
    const sugs = getSuggestions('label:', 6, agentsSchema, () => [
      'team=a,b',
      'team="x"',
      'team=web ui',
      'simple',
    ]);
    const byLabel = new Map(sugs.map((s) => [s.label, s.insertText]));
    expect(byLabel.get('team=a,b')).toBe('"team=a,b"');
    expect(byLabel.get('team="x"')).toBe('"team=\\"x\\""');
    expect(byLabel.get('team=web ui')).toBe('"team=web ui"');
    expect(byLabel.get('simple')).toBe('simple');
  });

  it('reports consistent UTF-16 offsets when non-ASCII input exceeds MAX_INPUT_BYTES', () => {
    // 257 copies of 'é' (2 UTF-8 bytes each = 514 bytes; 256 fit in 512 bytes).
    const input = 'é'.repeat(257);
    const res = parse(input, knownKeysFor(agentsSchema));
    expect(res.error?.code).toBe('limit');
    expect(res.error?.offset).toBe(256);
    expect(res.error?.length).toBe(1);
  });

  it('edits single-valued key for quick chips while preserving other terms and quoted whitespace', () => {
    expect(setSingleKeyInQuery('', 'phase', 'running', agentsSchema)).toBe('phase:running');
    expect(setSingleKeyInQuery('label:team=ui foo', 'phase', 'running', agentsSchema)).toBe(
      'phase:running label:team=ui foo'
    );
    expect(setSingleKeyInQuery('phase:error label:team=ui', 'phase', 'running', agentsSchema)).toBe(
      'phase:running label:team=ui'
    );
    expect(setSingleKeyInQuery('phase:running label:team=ui', 'phase', '', agentsSchema)).toBe(
      'label:team=ui'
    );
    expect(setSingleKeyInQuery('phase:running label:"a  b"', 'phase', 'error', agentsSchema)).toBe(
      'phase:error label:"a  b"'
    );
    expect(setSingleKeyInQuery('phase:running label:"a  b"', 'phase', '', agentsSchema)).toBe(
      'label:"a  b"'
    );
    expect(getActiveSingleKeyValue('phase:running label:a', 'phase', agentsSchema)).toBe('running');
    expect(getActiveSingleKeyValue('phase:running,error', 'phase', agentsSchema)).toBe('');
  });
});

describe('schema-to-docs completeness', () => {
  it('documents every key from schemas.generated.json in reference/filter-queries.md', () => {
    const doc = readFileSync(DOCS_PATH, 'utf8');
    for (const schema of VIEW_SCHEMAS) {
      expect(doc).toContain(`\`${schema.view}\``);
      for (const key of schema.keys) {
        expect(doc).toContain(`| \`${key.name}\` |`);
      }
    }
  });
});
