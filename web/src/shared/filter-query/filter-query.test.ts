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
  buildAgentValueSuggestions,
  canonical,
  getActiveSingleKeyValue,
  getSuggestions,
  getViewSchema,
  hasServerOnlyAgentTerms,
  knownKeysFor,
  match,
  matchAgentQuery,
  parse,
  serverSubset,
  setSingleKeyInQuery,
  toggleIsFlagInQuery,
  validate,
  type FilterRow,
  type Query,
  type Term,
  type ViewSchema,
} from './index.js';
import type { Agent } from '../types.js';

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
      activity?: string;
      project?: string;
      harness?: string;
      broker?: string;
      template?: string;
      owner?: string;
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
        if (key === 'activity') return r.activity ? [r.activity] : [];
        if (key === 'project') return r.project ? [r.project] : [];
        if (key === 'harness') return r.harness ? [r.harness] : [];
        if (key === 'broker') return r.broker ? [r.broker] : [];
        if (key === 'template') return r.template ? [r.template] : [];
        if (key === 'owner') return r.owner ? [r.owner] : [];
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

  it('strips client-only keys in serverSubset while preserving meaning when re-parsed on hub', () => {
    const known = knownKeysFor(agentsSchema);
    const checkSubset = (raw: string, wantSubset: string): void => {
      const res = parse(raw, known);
      expect(res.error).toBeUndefined();
      const q = res.query as Query;
      const sub = serverSubset(q, agentsSchema);
      expect(sub).toBe(wantSubset);
      if (sub !== '') {
        const reparsed = parse(sub, known);
        expect(reparsed.error).toBeUndefined();
        expect(reparsed.query).toBeDefined();
        if (q.mode === 'structured' && reparsed.query?.mode === 'text') {
          expect(reparsed.query.text).toBe(q.text ?? '');
          expect(q.excludeText ?? []).toEqual([]);
        }
      }
    };

    checkSubset('phase:running mode:lineage api', 'phase:running api');
    checkSubset('mode:lineage foo bar', 'foo bar');
    checkSubset('mode:lineage "a b"', 'a b');
    checkSubset('mode:lineage C:\\x', 'C:\\x');
    // Exclusions and key-shaped quoted text cannot be represented in pure-text
    // mode without a server key, so serverSubset returns '' (drain + client eval).
    checkSubset('mode:lineage -test', '');
    checkSubset('mode:lineage foo -bar', '');
    checkSubset('mode:lineage "phase:error"', '');
  });

  it('rejects reachability mode values (can_message / cannot_message) on project_agents schema', () => {
    const projectSchema = getViewSchema('project_agents') as ViewSchema;
    const known = knownKeysFor(projectSchema);

    const okRes = parse('mode:owner', known);
    expect(okRes.error).toBeUndefined();
    expect(validate(okRes.query as Query, projectSchema, 'mode:owner').error).toBeUndefined();

    for (const bad of ['mode:can_message', 'mode:cannot_message']) {
      const res = parse(bad, known);
      expect(res.error).toBeUndefined();
      const v = validate(res.query as Query, projectSchema, bad);
      expect(v.error?.code).toBe('value');
    }
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
    expect(getActiveSingleKeyValue('', 'phase', agentsSchema)).toBe('');
    expect(getActiveSingleKeyValue('label:a', 'phase', agentsSchema)).toBe('');
    expect(getActiveSingleKeyValue('phase:running label:a', 'phase', agentsSchema)).toBe('running');
    expect(getActiveSingleKeyValue('phase:running,error', 'phase', agentsSchema)).toBeNull();
    expect(getActiveSingleKeyValue('-phase:stopped', 'phase', agentsSchema)).toBeNull();
    expect(getActiveSingleKeyValue('phase:running phase:error', 'phase', agentsSchema)).toBeNull();
    expect(getActiveSingleKeyValue('phase:', 'phase', agentsSchema)).toBeNull();

    // Promoting pure text to structured mode preserves its literal meaning.
    expect(setSingleKeyInQuery('-foo', 'phase', 'running', agentsSchema)).toBe(
      'phase:running "-foo"'
    );
    expect(setSingleKeyInQuery('a OR b', 'phase', 'running', agentsSchema)).toBe(
      'phase:running "a OR b"'
    );
    expect(setSingleKeyInQuery('"hello"', 'phase', 'running', agentsSchema)).toBe(
      'phase:running "\\"hello\\""'
    );
    expect(setSingleKeyInQuery('foo', 'phase', 'running', agentsSchema)).toBe('phase:running foo');

    // Clearing the last chip unquotes clean pure text, and keeps remaining text
    // instead of silently dropping exclusions or terms that cannot be shown in
    // pure-text mode (P2 N3).
    expect(setSingleKeyInQuery('phase:running -test', 'phase', '', agentsSchema)).toBe('-test');
    expect(setSingleKeyInQuery('is:mine "phase:running"', 'is', '', agentsSchema)).toBe(
      '"phase:running"'
    );
    expect(setSingleKeyInQuery('is:mine "a b"', 'is', '', agentsSchema)).toBe('a b');
    expect(setSingleKeyInQuery('phase:running foo', 'phase', '', agentsSchema)).toBe('foo');

    // Artifacts toggleIsFlagInQuery promotes pure text with quoting and preserves text on clear.
    const artifactsSchema = getViewSchema('artifacts')!;
    expect(toggleIsFlagInQuery('-draft', 'review', [], artifactsSchema)).toBe('is:review "-draft"');
    expect(toggleIsFlagInQuery('a OR b', 'review', [], artifactsSchema)).toBe('is:review "a OR b"');
    expect(toggleIsFlagInQuery('"hello"', 'review', [], artifactsSchema)).toBe(
      'is:review "\\"hello\\""'
    );
    expect(toggleIsFlagInQuery('is:review -draft', 'review', [], artifactsSchema)).toBe('-draft');
    expect(toggleIsFlagInQuery('is:mine "a b"', 'mine', ['shared'], artifactsSchema)).toBe('a b');

    // Distinct referent cap: 20 distinct refs pass (duplicates fold), 21 fail with code 'limit'.
    const atCap =
      'project:p1,p2,p3,p4,p5,p6,p7,p8,p9,p10 broker:b1,b2,b3,b4,b5,b6,b7,b8,b9,b10 project:P1';
    const parsedAtCap = parse(atCap, knownKeysFor(agentsSchema));
    expect(parsedAtCap.error).toBeUndefined();
    expect(validate(parsedAtCap.query!, agentsSchema, atCap).error).toBeUndefined();

    const overCap = `${atCap} owner:o1`;
    const parsedOverCap = parse(overCap, knownKeysFor(agentsSchema));
    expect(parsedOverCap.error).toBeUndefined();
    const overErr = validate(parsedOverCap.query!, agentsSchema, overCap).error;
    expect(overErr?.code).toBe('limit');
  });

  it('matches all P2 agent keys and builds dynamic value suggestions from loaded rows', () => {
    const rows: Agent[] = [
      {
        id: '1',
        name: 'alpha-agent',
        slug: 'alpha-agent',
        projectId: 'p1',
        projectSlug: 'proj-alpha',
        template: 'developer',
        phase: 'running',
        activity: 'working',
        harnessConfig: 'claude',
        runtimeBrokerId: 'b1',
        runtimeBrokerName: 'broker-east',
        ownerId: 'u-me',
        messageMode: 'lineage',
        _messageability: { canMessage: true },
        labels: { team: 'ui', env: 'prod' },
      },
      {
        id: '2',
        name: 'beta-agent',
        slug: 'beta-agent',
        projectId: 'p2',
        projectSlug: 'proj-beta',
        template: 'reviewer',
        phase: 'stopped',
        harnessConfig: 'antigravity',
        runtimeBrokerId: 'b2',
        runtimeBrokerName: 'broker-west',
        ownerId: 'u-other',
        messageMode: 'none',
        _messageability: { canMessage: false },
      },
    ];
    const ctx = { currentUserId: 'u-me', currentUserEmail: 'me@example.com' };
    const known = knownKeysFor(agentsSchema);
    const evalIds = (q: string) => {
      const parsed = parse(q, known);
      expect(parsed.error).toBeUndefined();
      return rows.filter((a) => matchAgentQuery(parsed.query as Query, a, ctx)).map((a) => a.id);
    };

    expect(evalIds('activity:working')).toEqual(['1']);
    expect(evalIds('project:proj-alpha')).toEqual(['1']);
    expect(evalIds('harness:antigravity')).toEqual(['2']);
    expect(evalIds('broker:broker-east')).toEqual(['1']);
    expect(evalIds('template:reviewer')).toEqual(['2']);
    expect(evalIds('owner:me')).toEqual(['1']);
    expect(evalIds('mode:lineage')).toEqual(['1']);
    expect(evalIds('mode:can_message')).toEqual(['1']);
    expect(evalIds('mode:cannot_message')).toEqual(['2']);
    // is:mine is server-resolved and does not falsely hide loaded rows on the client.
    expect(evalIds('is:mine phase:running')).toEqual(['1']);

    // hasServerOnlyAgentTerms flags is: and external-email owner: terms, not owner:me or self email.
    expect(hasServerOnlyAgentTerms(parse('is:mine', known).query ?? null, ctx)).toBe(true);
    expect(hasServerOnlyAgentTerms(parse('owner:bob@example.com', known).query ?? null, ctx)).toBe(
      true
    );
    expect(hasServerOnlyAgentTerms(parse('owner:me', known).query ?? null, ctx)).toBe(false);
    expect(hasServerOnlyAgentTerms(parse('owner:me@example.com', known).query ?? null, ctx)).toBe(
      false
    );
    expect(hasServerOnlyAgentTerms(parse('phase:running', known).query ?? null, ctx)).toBe(false);

    const provider = buildAgentValueSuggestions(
      () => rows,
      () => ctx
    );
    expect(provider('project', '')).toEqual(expect.arrayContaining(['proj-alpha', 'proj-beta']));
    expect(provider('harness', '')).toEqual(expect.arrayContaining(['claude', 'antigravity']));
    expect(provider('broker', '')).toEqual(expect.arrayContaining(['broker-east', 'broker-west']));
    expect(provider('template', '')).toEqual(expect.arrayContaining(['developer', 'reviewer']));
    expect(provider('owner', '')).toEqual(expect.arrayContaining(['me', 'u-me', 'u-other']));
    expect(provider('label', '')).toEqual(expect.arrayContaining(['team', 'team=ui', 'env=prod']));
  });
});

describe('schema-to-docs completeness', () => {
  it('documents every key from schemas.generated.json in its view section of reference/filter-queries.md without experimental wording', () => {
    const doc = readFileSync(DOCS_PATH, 'utf8');
    expect(doc).not.toContain('filters.query_dialect');
    expect(doc.toLowerCase()).not.toContain('experimental');

    const sections = doc.split(/^### /m).slice(1);
    const byView = new Map<string, string>();
    for (const section of sections) {
      const match = /^`([^`]+)`/.exec(section);
      if (match) {
        byView.set(match[1], section);
      }
    }

    for (const schema of VIEW_SCHEMAS) {
      const section = byView.get(schema.view);
      expect(section, `missing ### \`${schema.view}\` section`).toBeDefined();
      for (const key of schema.keys) {
        expect(section, `missing key \`${key.name}\` in section \`${schema.view}\``).toContain(
          `| \`${key.name}\` |`
        );
      }
    }
  });
});
