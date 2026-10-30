import { describe, it, expect } from 'vitest';

import {
  classifySettingsError,
  fallbackFieldKey,
  sectionPathsFromSchema,
  fieldErrorMessages,
  genericSaveErrorText,
  readSettingsSaveError,
  resolveFieldKey,
} from './settings-save-errors.js';

// Error bodies below mirror what pkg/hub sends for admin settings saves
// (ptone/scion#3923): flat {error: "<code>", ...} from the server-config PUT
// and nested {error: {code, message, details}} from writeError.

// Section -> koanf paths as GET /api/v1/admin/server-config/schema serves
// them (subset of pkg/config/opsettings/registry.go).
const SECTION_PATHS = {
  access: [
    'server.hub.admin_emails',
    'server.auth.user_access_mode',
    'server.auth.default_user_role',
    'server.auth.authorized_domains',
  ],
  endpoints: ['server.hub.public_url', 'image_registry'],
  // Order as in registry.go; telemetry.enabled is deliberately not first
  // here to show the match does not depend on order.
  telemetry: [
    'telemetry.cloud',
    'telemetry.cloud.enabled',
    'telemetry.enabled',
    'telemetry.cloud.endpoint',
  ],
  federation: ['server.federation.refresh_interval', 'server.federation.trusted_issuers'],
  runtimes: ['runtimes'],
};
const CTX = { sectionKoanfPaths: SECTION_PATHS };

describe('classifySettingsError — validation', () => {
  it('reads a flat validation_failed body with capitalized Path/Message, as the hub sends it', () => {
    const err = classifySettingsError(
      400,
      {
        error: 'validation_failed',
        errors: {
          endpoints: [{ Path: 'public_url', Message: 'must be an absolute URL' }],
          telemetry: [{ Path: 'cloud/endpoint', Message: 'is required' }],
        },
      },
      CTX
    );
    expect(err.kind).toBe('validation');
    expect(err.fieldErrors).toEqual([
      {
        section: 'endpoints',
        key: 'server.hub.public_url',
        path: 'public_url',
        message: 'must be an absolute URL',
      },
      {
        section: 'telemetry',
        key: 'telemetry.cloud.endpoint',
        path: 'cloud/endpoint',
        message: 'is required',
      },
    ]);
  });

  it('accepts lowercase path/field/message keys', () => {
    const err = classifySettingsError(
      400,
      {
        error: 'validation_failed',
        errors: {
          access: [
            { path: 'admin_emails/1', message: 'invalid email' },
            { field: 'default_user_role', message: 'unknown role' },
          ],
        },
      },
      CTX
    );
    expect(err.fieldErrors.map((e) => [e.key, e.message])).toEqual([
      ['server.hub.admin_emails.1', 'invalid email'],
      ['server.auth.default_user_role', 'unknown role'],
    ]);
  });

  it('keeps a root-level entry (empty Path) as a section-level error', () => {
    const err = classifySettingsError(400, {
      error: 'validation_failed',
      errors: { agent_defaults: [{ Path: '', Message: 'additional properties not allowed' }] },
    });
    expect(err.fieldErrors).toEqual([
      {
        section: 'agent_defaults',
        key: null,
        path: '',
        message: 'additional properties not allowed',
      },
    ]);
  });

  it('uses the schema endpoint mapping when given, and map sections nest below the prefix', () => {
    const ctx = {
      sectionKoanfPaths: { endpoints: ['server.hub.public_url'], runtimes: ['runtimes'] },
    };
    expect(resolveFieldKey('endpoints', 'public_url', ctx)).toBe('server.hub.public_url');
    expect(resolveFieldKey('runtimes', 'docker/type', ctx)).toBe('runtimes.docker.type');
    expect(resolveFieldKey('endpoints', 'unknown_leaf', ctx)).toBeNull();
    expect(resolveFieldKey(null, 'server.hub.port')).toBe('server.hub.port');
  });

  it('reads a nested validation_error with no field as a section-level entry, without parsing the message', () => {
    const err = classifySettingsError(
      422,
      {
        error: {
          code: 'validation_error',
          message: 'invalid default_timezone "Not/A/Zone": unknown time zone Not/A/Zone',
        },
      },
      {}
    );
    expect(err.kind).toBe('validation');
    expect(err.fieldErrors).toEqual([
      {
        section: null,
        key: null,
        path: '',
        message: 'invalid default_timezone "Not/A/Zone": unknown time zone Not/A/Zone',
      },
    ]);
  });

  it('maps a nested validation_error whose details name the field', () => {
    const err = classifySettingsError(
      422,
      {
        error: {
          code: 'validation_error',
          message: 'must be a duration',
          details: { section: 'federation', field: 'refresh_interval' },
        },
      },
      CTX
    );
    expect(err.fieldErrors).toEqual([
      {
        section: 'federation',
        key: 'server.federation.refresh_interval',
        path: 'refresh_interval',
        message: 'must be a duration',
      },
    ]);
  });

  it('lists details.errors strings of a nested validation_error under the default section', () => {
    const err = classifySettingsError(
      422,
      {
        error: {
          code: 'validation_error',
          message: 'federation config validation failed',
          details: { errors: ['trusted_issuers[0]: issuer_url is required'] },
        },
      },
      { defaultSection: 'federation' }
    );
    expect(err.fieldErrors.map((e) => [e.section, e.key, e.message])).toEqual([
      ['federation', null, 'federation config validation failed'],
      ['federation', null, 'trusted_issuers[0]: issuer_url is required'],
    ]);
  });
});

describe('section path mapping', () => {
  it('without the schema mapping, a section-relative path maps to no field (summary only)', () => {
    const err = classifySettingsError(400, {
      error: 'validation_failed',
      errors: { endpoints: [{ Path: 'public_url', Message: 'bad' }] },
    });
    expect(err.fieldErrors[0]).toMatchObject({
      section: 'endpoints',
      key: null,
      path: 'public_url',
    });
  });

  it('prefers the match with the fewest segments, independent of order', () => {
    expect(resolveFieldKey('telemetry', 'enabled', CTX)).toBe('telemetry.enabled');
    expect(resolveFieldKey('telemetry', 'cloud/enabled', CTX)).toBe('telemetry.cloud.enabled');
    expect(resolveFieldKey('telemetry', 'cloud/tls/enabled', CTX)).toBe(
      'telemetry.cloud.tls.enabled'
    );
  });

  it('decodes JSON-pointer escapes in each segment', () => {
    expect(resolveFieldKey('runtimes', 'a~1b/type', CTX)).toBe('runtimes.a/b.type');
    expect(fallbackFieldKey('x', 'a~0b')).toBe('x.a~b');
  });

  it('sectionPathsFromSchema reads the schema body and ignores malformed entries', () => {
    expect(
      sectionPathsFromSchema({
        sections: {
          endpoints: { koanf_paths: ['server.hub.public_url', 7, ''] },
          empty: { koanf_paths: [] },
          bad: 'x',
        },
      })
    ).toEqual({ endpoints: ['server.hub.public_url'] });
    expect(sectionPathsFromSchema(null)).toBeUndefined();
    expect(sectionPathsFromSchema({ sections: {} })).toBeUndefined();
  });
});

describe('fallbackFieldKey', () => {
  it('places pointer, dotted and leading-slash paths under the prefix', () => {
    expect(fallbackFieldKey('server.github_app', 'private_key')).toBe(
      'server.github_app.private_key'
    );
    expect(fallbackFieldKey('server.github_app', '/private_key')).toBe(
      'server.github_app.private_key'
    );
    expect(fallbackFieldKey('settings', 'a/b')).toBe('settings.a.b');
    expect(fallbackFieldKey(null, 'bot_token')).toBe('bot_token');
    expect(fallbackFieldKey('settings', '')).toBeNull();
  });
});

describe('classifySettingsError — conflict and Layer-0', () => {
  it('reads a 409 revision_conflict with its conflicted sections', () => {
    const err = classifySettingsError(409, {
      error: 'revision_conflict',
      message: 'One or more sections have been modified since the expected revision.',
      applied: {},
      conflicted: [{ section: 'access', expected_revision: 3, current_revision: 5 }, 'junk'],
    });
    expect(err.kind).toBe('conflict');
    expect(err.message).toBe(
      'One or more sections have been modified since the expected revision.'
    );
    expect(err.conflicted).toEqual([
      { section: 'access', expected_revision: 3, current_revision: 5 },
    ]);
  });

  it('reads the keys a 409 saved before the conflict (ptone/scion#4467)', () => {
    const err = classifySettingsError(409, {
      error: 'revision_conflict',
      conflicted: [{ section: 'access', expected_revision: 3, current_revision: 5 }],
      applied_keys: ['server.hub.hub_name', '', 7],
      pending_restart: ['server.hub.public_url'],
      skipped: [
        { key: 'server.notifications.slack', reason: 'invalid webhook URL' },
        { key: 'server.x' },
        { reason: 'no key' },
        'junk',
      ],
    });
    expect(err.kind).toBe('conflict');
    expect(err.appliedKeys).toEqual(['server.hub.hub_name']);
    expect(err.pendingRestart).toEqual(['server.hub.public_url']);
    expect(err.skipped).toEqual([
      { key: 'server.notifications.slack', reason: 'invalid webhook URL' },
      { key: 'server.x', reason: '' },
    ]);
  });

  it('a 409 without saved keys, and other kinds, report none', () => {
    const conflict = classifySettingsError(409, { error: 'revision_conflict', conflicted: [] });
    expect(conflict.appliedKeys).toEqual([]);
    expect(conflict.pendingRestart).toEqual([]);
    expect(conflict.skipped).toEqual([]);
    const generic = classifySettingsError(500, { error: 'boom', pending_restart: ['x'] });
    expect(generic.pendingRestart).toEqual([]);
  });

  it('reads a 422 layer0_rejected with its keys', () => {
    const err = classifySettingsError(422, {
      error: 'layer0_rejected',
      message:
        'Bootstrap settings are managed via settings.yaml / deployment tooling; restart required.',
      keys: ['server.database.driver', 'server.mode'],
    });
    expect(err.kind).toBe('layer0');
    expect(err.keys).toEqual(['server.database.driver', 'server.mode']);
  });
});

describe('classifySettingsError — fallback', () => {
  for (const [label, body] of [
    ['undefined (not JSON)', undefined],
    ['null', null],
    ['a bare string', 'Bad Gateway'],
    ['an array', [{ error: 'validation_failed' }]],
    ['an empty object', {}],
  ] as const) {
    it(`treats ${label} as a generic error with no message`, () => {
      const err = classifySettingsError(502, body);
      expect(err.kind).toBe('generic');
      expect(err.message).toBeNull();
      expect(genericSaveErrorText(err, 'Failed to save settings')).toBe('Failed to save settings');
    });
  }

  it('treats an unknown code as generic and keeps the server message', () => {
    const err = classifySettingsError(500, {
      error: { code: 'internal_error', message: 'Failed to update section "access"' },
    });
    expect(err.kind).toBe('generic');
    expect(genericSaveErrorText(err, 'Failed to save settings')).toBe(
      'Failed to save settings: Failed to update section "access"'
    );
  });

  it('treats a validation code with malformed entries and no message as generic', () => {
    const err = classifySettingsError(400, {
      error: 'validation_failed',
      errors: { access: [42, { Path: 'admin_emails' }] },
    });
    expect(err.kind).toBe('generic');
  });

  it('reads a non-JSON response body as generic', async () => {
    const res = new Response('<html>502</html>', { status: 502 });
    const err = await readSettingsSaveError(res);
    expect(err.kind).toBe('generic');
    expect(err.status).toBe(502);
  });
});

describe('fieldErrorMessages', () => {
  it('matches the field key and keys below it, not sibling prefixes', () => {
    const errors = [
      { section: 'x', key: 'server.hub.admin_emails.1', path: '', message: 'a' },
      { section: 'x', key: 'server.hub.admin_emails', path: '', message: 'b' },
      { section: 'x', key: 'server.hub.admin_emails_extra', path: '', message: 'c' },
      { section: 'x', key: null, path: '', message: 'd' },
    ];
    expect(fieldErrorMessages(errors, 'server.hub.admin_emails')).toEqual(['a', 'b']);
    expect(fieldErrorMessages(null, 'server.hub.admin_emails')).toEqual([]);
  });
});
