import { describe, it, expect } from 'vitest';

import {
  classifySettingsError,
  fieldErrorMessages,
  genericSaveErrorText,
  readSettingsSaveError,
  resolveFieldKey,
} from './settings-save-errors.js';

// Error bodies below mirror what pkg/hub sends for admin settings saves
// (ptone/scion#3923): flat {error: "<code>", ...} from the server-config PUT
// and nested {error: {code, message, details}} from writeError.

describe('classifySettingsError — validation', () => {
  it('reads a flat validation_failed body with capitalized Path/Message, as the hub sends it', () => {
    const err = classifySettingsError(400, {
      error: 'validation_failed',
      errors: {
        endpoints: [{ Path: 'public_url', Message: 'must be an absolute URL' }],
        telemetry: [{ Path: 'cloud/endpoint', Message: 'is required' }],
      },
    });
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
    const err = classifySettingsError(400, {
      error: 'validation_failed',
      errors: {
        access: [
          { path: 'admin_emails/1', message: 'invalid email' },
          { field: 'default_user_role', message: 'unknown role' },
        ],
      },
    });
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
    const err = classifySettingsError(422, {
      error: {
        code: 'validation_error',
        message: 'must be a duration',
        details: { section: 'federation', field: 'refresh_interval' },
      },
    });
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
