import { describe, it, expect } from 'vitest';

import {
  dirtySectionsPayload,
  payloadLeaves,
  revisionsFromMetadata,
  sectionOfPath,
} from './settings-dirty.js';

// Section -> koanf paths as the schema endpoint serves them (subset).
const SECTIONS: Record<string, string[]> = {
  access: ['server.hub.admin_emails', 'server.auth.user_access_mode'],
  endpoints: ['server.hub.public_url', 'image_registry'],
  telemetry: ['telemetry.enabled', 'telemetry.cloud', 'telemetry.cloud.endpoint'],
  runtimes: ['runtimes'],
  agent_defaults: ['default_template', 'default_max_turns'],
};

const BASE = {
  default_template: 'claude',
  image_registry: 'ghcr.io/x',
  server: {
    hub: { admin_emails: ['a@example.com'], public_url: 'https://hub.example.com' },
    auth: { user_access_mode: 'open' },
  },
  telemetry: { enabled: true, cloud: { endpoint: 'otel:4317', headers: { k: '********' } } },
  runtimes: { docker: { type: 'docker' } },
};

const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v)) as T;

describe('payloadLeaves', () => {
  it('stops at koanf paths and at non-objects', () => {
    const leaves = payloadLeaves(BASE, new Set(Object.values(SECTIONS).flat()));
    expect([...leaves.keys()].sort()).toEqual([
      'default_template',
      'image_registry',
      'runtimes',
      'server.auth.user_access_mode',
      'server.hub.admin_emails',
      'server.hub.public_url',
      'telemetry.cloud',
      'telemetry.enabled',
    ]);
    expect(leaves.get('runtimes')).toEqual({ docker: { type: 'docker' } });
  });
});

describe('sectionOfPath', () => {
  it('maps exact, below and above matches, longest first', () => {
    expect(sectionOfPath('server.hub.public_url', SECTIONS)).toBe('endpoints');
    expect(sectionOfPath('runtimes.docker.type', SECTIONS)).toBe('runtimes');
    expect(sectionOfPath('telemetry.cloud', SECTIONS)).toBe('telemetry');
    expect(sectionOfPath('server.hub.nope', SECTIONS)).toBeNull();
  });
});

describe('revisionsFromMetadata', () => {
  it('uses the stored revision for a db row and 0 for no row', () => {
    expect(
      revisionsFromMetadata({
        access: { source: 'db', revision: 7 },
        endpoints: { source: 'file' },
        telemetry: { source: 'default' },
      })
    ).toEqual({ access: 7, endpoints: 0, telemetry: 0 });
    expect(revisionsFromMetadata(null)).toEqual({});
  });
});

describe('dirtySectionsPayload', () => {
  const revs = { access: 4, endpoints: 0, telemetry: 9, runtimes: 2, agent_defaults: 3 };

  it('an unchanged payload sends nothing', () => {
    const d = dirtySectionsPayload(clone(BASE), BASE, SECTIONS, revs);
    expect(d.payload).toEqual({});
    expect(d.sections).toEqual([]);
    expect(d.expectedRevisions).toEqual({});
  });

  it('a changed field sends its whole section, and untouched sections are absent', () => {
    const cur = clone(BASE);
    cur.server.hub.public_url = 'https://new.example.com';
    const d = dirtySectionsPayload(cur, BASE, SECTIONS, revs);
    expect(d.sections).toEqual(['endpoints']);
    expect(d.payload).toEqual({
      image_registry: 'ghcr.io/x',
      server: { hub: { public_url: 'https://new.example.com' } },
    });
    expect(d.expectedRevisions).toEqual({ endpoints: 0 });
    expect(d.payload).not.toHaveProperty('telemetry');
    expect(d.payload).not.toHaveProperty('runtimes');
  });

  it('a nested change inside a koanf leaf (masked header block) sends the section whole', () => {
    const cur = clone(BASE);
    cur.telemetry.cloud.endpoint = 'otel2:4317';
    const d = dirtySectionsPayload(cur, BASE, SECTIONS, revs);
    expect(d.sections).toEqual(['telemetry']);
    expect(d.payload).toEqual({
      telemetry: {
        enabled: true,
        cloud: { endpoint: 'otel2:4317', headers: { k: '********' } },
      },
    });
    expect(d.expectedRevisions).toEqual({ telemetry: 9 });
  });

  it('several changed sections each get their revision; key order does not matter', () => {
    const cur = clone(BASE);
    cur.runtimes = { docker: { type: 'docker' }, k8s: { type: 'kubernetes' } };
    cur.default_template = 'gemini';
    const reordered = {
      runtimes: cur.runtimes,
      telemetry: cur.telemetry,
      server: cur.server,
      image_registry: cur.image_registry,
      default_template: cur.default_template,
    };
    const d = dirtySectionsPayload(reordered, BASE, SECTIONS, revs);
    expect(d.sections).toEqual(['agent_defaults', 'runtimes']);
    expect(d.expectedRevisions).toEqual({ agent_defaults: 3, runtimes: 2 });
  });

  it('a field that appears or disappears marks its section changed', () => {
    const cur = clone(BASE) as Record<string, unknown>;
    cur.default_max_turns = 10;
    expect(dirtySectionsPayload(cur, BASE, SECTIONS, revs).sections).toEqual(['agent_defaults']);
  });

  it('a changed leaf no section claims is sent on its own, with no revision', () => {
    const base = { ...clone(BASE), extra: { x: 1 } };
    const cur = { ...clone(BASE), extra: { x: 2 } };
    const d = dirtySectionsPayload(cur, base, SECTIONS, revs);
    expect(d.payload).toEqual({ extra: { x: 2 } });
    expect(d.sections).toEqual([]);
    expect(d.expectedRevisions).toEqual({});
  });
});
