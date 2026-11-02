import { describe, it, expect } from 'vitest';
import {
  MASKED_ENV_VALUE,
  envValueFor,
  envValuesUnder,
  formatEnvValue,
  hasInEffect,
} from './admin-server-config-env-values.js';

describe('formatEnvValue', () => {
  it('joins lists, prints scalars and objects', () => {
    expect(formatEnvValue({ value: ['a.example', 'b.example'], in_effect: true })).toBe(
      'a.example, b.example'
    );
    expect(formatEnvValue({ value: 9090, in_effect: true })).toBe('9090');
    expect(formatEnvValue({ value: false, in_effect: true })).toBe('false');
    expect(formatEnvValue({ value: { a: 1 }, in_effect: true })).toBe('{"a":1}');
    expect(formatEnvValue({ in_effect: true })).toBe('');
  });

  it('a masked value is always the placeholder', () => {
    expect(formatEnvValue({ value: 'raw-secret', masked: true, in_effect: true })).toBe(
      MASKED_ENV_VALUE
    );
    expect(formatEnvValue({ masked: true, in_effect: false })).toBe(MASKED_ENV_VALUE);
  });
});

describe('hasInEffect', () => {
  it('is true only for an unmasked entry with a reported state', () => {
    expect(hasInEffect({ value: 'x', in_effect: true })).toBe(true);
    expect(hasInEffect({ value: 'x', in_effect: false })).toBe(true);
    expect(hasInEffect({ value: '********', masked: true })).toBe(false);
    expect(hasInEffect({ value: '********', masked: true, in_effect: true })).toBe(false);
    expect(hasInEffect({ value: 'x' })).toBe(false);
  });
});

describe('envValueFor / envValuesUnder', () => {
  const values = {
    'server.hub.port': { value: 1, in_effect: true },
    'profiles.local.runtime': { value: '********', masked: true, in_effect: false },
    'profiles.local.image_registry': { value: '********', masked: true, in_effect: true },
    'profilesx.other': { value: 'x', in_effect: true },
  };

  it('returns the first reported key', () => {
    expect(envValueFor(values, ['nope', 'server.hub.port'])?.key).toBe('server.hub.port');
    expect(envValueFor(values, ['nope'])).toBeNull();
    expect(envValueFor(undefined, ['server.hub.port'])).toBeNull();
  });

  it('lists the keys at or under a prefix, sorted', () => {
    expect(envValuesUnder(values, 'profiles').map((e) => e.key)).toEqual([
      'profiles.local.image_registry',
      'profiles.local.runtime',
    ]);
    expect(envValuesUnder(null, 'profiles')).toEqual([]);
  });
});
