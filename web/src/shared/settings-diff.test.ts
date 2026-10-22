import { describe, it, expect } from 'vitest';
import { diffPayload } from './settings-diff.js';

describe('diffPayload (ptone/scion#3913)', () => {
  it('returns nothing when nothing changed', () => {
    expect(diffPayload({ a: 1, b: ['x'], c: { d: '' } }, { a: 1, b: ['x'], c: { d: '' } })).toEqual(
      {}
    );
  });

  it('returns changed scalars, and arrays as a whole', () => {
    expect(diffPayload({ a: 2, b: ['x', 'y'], c: 'same' }, { a: 1, b: ['x'], c: 'same' })).toEqual({
      a: 2,
      b: ['x', 'y'],
    });
  });

  it('compares plain objects key by key', () => {
    expect(
      diffPayload({ o: { keep: 1, change: 'new', add: true } }, { o: { keep: 1, change: 'old' } })
    ).toEqual({ o: { change: 'new', add: true } });
  });

  it('sends cleared values explicitly', () => {
    expect(diffPayload({ s: '', l: [], f: false }, { s: 'x', l: ['y'], f: true })).toEqual({
      s: '',
      l: [],
      f: false,
    });
  });

  it('sends a key that is new in current', () => {
    expect(diffPayload({ n: null, k: 'v' }, {})).toEqual({ n: null, k: 'v' });
  });
});
