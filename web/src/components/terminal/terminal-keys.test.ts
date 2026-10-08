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

import { describe, expect, it } from 'vitest';
import {
  applyModifiers,
  barKeySequence,
  consumeModifier,
  ctrlCode,
  nextModifierState,
  type TerminalBarKey,
} from './terminal-keys.js';

const CTRL = { ctrl: true, alt: false };
const ALT = { ctrl: false, alt: true };
const CTRL_ALT = { ctrl: true, alt: true };

describe('barKeySequence', () => {
  it.each<[TerminalBarKey, string, string]>([
    ['up', '\x1b[A', '\x1bOA'],
    ['down', '\x1b[B', '\x1bOB'],
    ['right', '\x1b[C', '\x1bOC'],
    ['left', '\x1b[D', '\x1bOD'],
    ['home', '\x1b[H', '\x1bOH'],
    ['end', '\x1b[F', '\x1bOF'],
  ])('%s follows DECCKM: CSI normally, SS3 in application cursor mode', (key, normal, app) => {
    expect(barKeySequence(key, false)).toBe(normal);
    expect(barKeySequence(key, true)).toBe(app);
  });

  it.each<[TerminalBarKey, string]>([
    ['escape', '\x1b'],
    ['tab', '\t'],
    ['backtab', '\x1b[Z'],
    ['pageup', '\x1b[5~'],
    ['pagedown', '\x1b[6~'],
  ])('%s does not depend on the cursor mode', (key, sequence) => {
    expect(barKeySequence(key, false)).toBe(sequence);
    expect(barKeySequence(key, true)).toBe(sequence);
  });

  it('modified cursor keys use the CSI form with xterm modifier parameters in both modes', () => {
    for (const app of [false, true]) {
      expect(barKeySequence('up', app, CTRL)).toBe('\x1b[1;5A');
      expect(barKeySequence('left', app, ALT)).toBe('\x1b[1;3D');
      expect(barKeySequence('right', app, CTRL_ALT)).toBe('\x1b[1;7C');
      expect(barKeySequence('home', app, CTRL)).toBe('\x1b[1;5H');
    }
    expect(barKeySequence('pageup', false, CTRL)).toBe('\x1b[5;5~');
    expect(barKeySequence('pagedown', false, ALT)).toBe('\x1b[6;3~');
  });

  it('Alt prefixes ESC to Esc and Tab, Ctrl leaves them unchanged', () => {
    expect(barKeySequence('escape', false, ALT)).toBe('\x1b\x1b');
    expect(barKeySequence('tab', false, ALT)).toBe('\x1b\t');
    expect(barKeySequence('escape', false, CTRL)).toBe('\x1b');
    expect(barKeySequence('tab', false, CTRL)).toBe('\t');
  });
});

describe('ctrlCode', () => {
  it('maps letters of either case to 0x01-0x1a', () => {
    expect(ctrlCode('a')).toBe('\x01');
    expect(ctrlCode('c')).toBe('\x03');
    expect(ctrlCode('C')).toBe('\x03');
    expect(ctrlCode('z')).toBe('\x1a');
  });

  it('maps space, @ and [ \\ ] ^ _ ? like a terminal keyboard', () => {
    expect(ctrlCode(' ')).toBe('\x00');
    expect(ctrlCode('@')).toBe('\x00');
    expect(ctrlCode('[')).toBe('\x1b');
    expect(ctrlCode('\\')).toBe('\x1c');
    expect(ctrlCode(']')).toBe('\x1d');
    expect(ctrlCode('^')).toBe('\x1e');
    expect(ctrlCode('_')).toBe('\x1f');
    expect(ctrlCode('?')).toBe('\x7f');
  });

  it('maps the digits 2-8 like xterm.js: NUL, ESC, FS, GS, RS, US, DEL', () => {
    expect(['2', '3', '4', '5', '6', '7', '8'].map(ctrlCode)).toEqual([
      '\x00',
      '\x1b',
      '\x1c',
      '\x1d',
      '\x1e',
      '\x1f',
      '\x7f',
    ]);
  });

  it('maps Backspace (DEL) to BS, like xterm.js', () => {
    expect(ctrlCode('\x7f')).toBe('\b');
  });

  it('has no code for 0, 1, 9, other punctuation or non-ASCII letters', () => {
    for (const char of ['0', '1', '9', '/', '|', '~', 'é', '\r']) expect(ctrlCode(char)).toBeNull();
  });
});

describe('applyModifiers', () => {
  it('maps one typed character and reports the modifier consumed', () => {
    expect(applyModifiers('c', CTRL)).toEqual({ data: '\x03', consumed: true });
    expect(applyModifiers('b', ALT)).toEqual({ data: '\x1bb', consumed: true });
    expect(applyModifiers('x', CTRL_ALT)).toEqual({ data: '\x1b\x18', consumed: true });
    expect(applyModifiers('\r', ALT)).toEqual({ data: '\x1b\r', consumed: true });
    expect(applyModifiers('\x7f', CTRL)).toEqual({ data: '\b', consumed: true });
    expect(applyModifiers('\x7f', ALT)).toEqual({ data: '\x1b\x7f', consumed: true });
  });

  it('sends a character with no control code as itself, still consuming Ctrl', () => {
    expect(applyModifiers('1', CTRL)).toEqual({ data: '1', consumed: true });
  });

  it('treats an astral character as one character', () => {
    expect(applyModifiers('🙂', ALT)).toEqual({ data: '\x1b🙂', consumed: true });
  });

  it('leaves multi-character data and unmodified input alone', () => {
    expect(applyModifiers('hello', CTRL)).toEqual({ data: 'hello', consumed: false });
    expect(applyModifiers('\x1b[12;5R', CTRL)).toEqual({ data: '\x1b[12;5R', consumed: false });
    expect(applyModifiers('c', { ctrl: false, alt: false })).toEqual({
      data: 'c',
      consumed: false,
    });
  });
});

describe('sticky modifier state', () => {
  it('cycles off → armed → locked → off', () => {
    expect(nextModifierState('off')).toBe('armed');
    expect(nextModifierState('armed')).toBe('locked');
    expect(nextModifierState('locked')).toBe('off');
  });

  it('a key clears an armed modifier and keeps a locked one', () => {
    expect(consumeModifier('armed')).toBe('off');
    expect(consumeModifier('locked')).toBe('locked');
    expect(consumeModifier('off')).toBe('off');
  });
});
