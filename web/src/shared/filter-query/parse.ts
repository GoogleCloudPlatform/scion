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

import {
  MAX_INPUT_BYTES,
  MAX_TERMS,
  MAX_TEXT_RUNES,
  MAX_VALUES_PER_KEY,
  type FilterError,
  type ParseResult,
  type Term,
} from './types.js';

const utf8Encoder = new TextEncoder();

function utf8ByteLength(s: string): number {
  return utf8Encoder.encode(s).length;
}

function runeCount(s: string): number {
  let count = 0;
  for (const _ of s) {
    void _;
    count++;
  }
  return count;
}

function isSpace(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r';
}

function isASCIILetter(code: number): boolean {
  return (code >= 65 && code <= 90) || (code >= 97 && code <= 122);
}

function isKeyChar(code: number): boolean {
  return isASCIILetter(code) || (code >= 48 && code <= 57) || code === 95;
}

export function scanKeyPrefix(
  input: string,
  pos: number
): { key: string; colonPos: number } | null {
  const n = input.length;
  if (pos >= n || !isASCIILetter(input.charCodeAt(pos))) {
    return null;
  }
  let j = pos + 1;
  while (j < n && isKeyChar(input.charCodeAt(j))) {
    j++;
  }
  if (j < n && input[j] === ':') {
    return { key: input.slice(pos, j), colonPos: j };
  }
  return null;
}

export function comparisonPrefixLen(s: string): number {
  if (s.startsWith('>=') || s.startsWith('<=')) return 2;
  if (s.startsWith('>') || s.startsWith('<')) return 1;
  return 0;
}

function hasKnownKeyTerm(input: string, known: (key: string) => boolean): boolean {
  const n = input.length;
  let i = 0;
  while (i < n) {
    while (i < n && isSpace(input[i])) i++;
    if (i >= n) break;
    if (input[i] === '"') {
      i++;
      while (i < n) {
        if (input[i] === '\\' && i + 1 < n) {
          i += 2;
          continue;
        }
        if (input[i] === '"') {
          i++;
          break;
        }
        i++;
      }
      continue;
    }
    let start = i;
    if (input[start] === '-') start++;
    const prefix = scanKeyPrefix(input, start);
    if (prefix && known(prefix.key.toLowerCase())) {
      return true;
    }
    while (i < n && !isSpace(input[i])) i++;
  }
  return false;
}

function parseQuoted(
  input: string,
  pos: number
): { value: string; next: number } | { error: FilterError } {
  let out = '';
  const n = input.length;
  let i = pos + 1;
  while (i < n) {
    const ch = input[i];
    if (ch === '"') {
      return { value: out, next: i + 1 };
    }
    if (ch === '\\') {
      if (i + 1 >= n) {
        return {
          error: {
            code: 'syntax',
            message: 'unterminated quote',
            offset: pos,
            length: n - pos,
          },
        };
      }
      const next = input[i + 1];
      if (next !== '"' && next !== '\\') {
        return {
          error: {
            code: 'syntax',
            message: `invalid escape sequence '\\${next}'; only \\" and \\\\ are allowed`,
            offset: i,
            length: 2,
          },
        };
      }
      out += next;
      i += 2;
      continue;
    }
    out += ch;
    i++;
  }
  return {
    error: {
      code: 'syntax',
      message: 'unterminated quote',
      offset: pos,
      length: n - pos,
    },
  };
}

/**
 * Parse a filter query string using the unified dialect.
 * If no known key term appears in input, returns pure-text mode with the
 * trimmed input verbatim (no quote or `-` processing).
 */
export function parse(input: string, known: (key: string) => boolean): ParseResult {
  const byteLen = utf8ByteLength(input);
  if (byteLen > MAX_INPUT_BYTES) {
    return {
      error: {
        code: 'limit',
        message: `filter query exceeds maximum length of ${MAX_INPUT_BYTES} bytes`,
        offset: MAX_INPUT_BYTES,
        length: byteLen - MAX_INPUT_BYTES,
      },
    };
  }

  const trimmed = input.trim();
  if (trimmed === '') {
    return { query: { mode: 'text', text: '' } };
  }

  if (!hasKnownKeyTerm(input, known)) {
    if (runeCount(trimmed) > MAX_TEXT_RUNES) {
      return {
        error: {
          code: 'limit',
          message: `search text exceeds maximum length of ${MAX_TEXT_RUNES} characters`,
          offset: 0,
          length: input.length,
        },
      };
    }
    return { query: { mode: 'text', text: trimmed } };
  }

  const terms: Term[] = [];
  const posTextTokens: string[] = [];
  const excludeText: string[] = [];

  const n = input.length;
  let i = 0;

  while (i < n) {
    while (i < n && isSpace(input[i])) i++;
    if (i >= n) break;

    const termStart = i;
    let negate = false;
    if (input[i] === '-') {
      negate = true;
      i++;
      if (i >= n || isSpace(input[i])) {
        return {
          error: {
            code: 'syntax',
            message: "expected term after '-'",
            offset: termStart,
            length: 1,
          },
        };
      }
    }

    if (input[i] === '(' || input[i] === ')') {
      return {
        error: {
          code: 'syntax',
          message: 'parentheses are not supported',
          offset: i,
          length: 1,
        },
      };
    }

    if (input[i] === '"') {
      const qRes = parseQuoted(input, i);
      if ('error' in qRes) return { error: qRes.error };
      i = qRes.next;
      if (i < n && !isSpace(input[i])) {
        return {
          error: {
            code: 'syntax',
            message: 'unexpected character after closing quote',
            offset: i,
            length: 1,
          },
        };
      }
      if (negate) {
        excludeText.push(qRes.value);
      } else {
        posTextTokens.push(qRes.value);
      }
      continue;
    }

    const prefix = scanKeyPrefix(input, i);
    if (prefix && known(prefix.key.toLowerCase())) {
      const lowerKey = prefix.key.toLowerCase();
      i = prefix.colonPos + 1;
      if (i >= n || isSpace(input[i])) {
        return {
          error: {
            code: 'syntax',
            message: `expected value after "${prefix.key}:"`,
            offset: termStart,
            length: i - termStart,
          },
        };
      }

      const values: string[] = [];
      for (;;) {
        const valStart = i;
        if (i >= n || isSpace(input[i])) {
          return {
            error: {
              code: 'syntax',
              message: "expected value after ','",
              offset: valStart - 1,
              length: 1,
            },
          };
        }
        if (input[i] === ',') {
          return {
            error: {
              code: 'syntax',
              message: 'empty value in comma-separated list',
              offset: i,
              length: 1,
            },
          };
        }
        if (input[i] === '(' || input[i] === ')') {
          return {
            error: {
              code: 'syntax',
              message: 'parentheses are not supported',
              offset: i,
              length: 1,
            },
          };
        }

        if (input[i] === '"') {
          const qRes = parseQuoted(input, i);
          if ('error' in qRes) return { error: qRes.error };
          i = qRes.next;
          if (i < n && !isSpace(input[i]) && input[i] !== ',') {
            return {
              error: {
                code: 'syntax',
                message: 'unexpected character after closing quote',
                offset: i,
                length: 1,
              },
            };
          }
          values.push(qRes.value);
        } else {
          const opLen = comparisonPrefixLen(input.slice(i));
          if (opLen > 0) {
            return {
              error: {
                code: 'syntax',
                message: 'comparison operators are not supported yet',
                offset: valStart,
                length: opLen,
              },
            };
          }
          while (
            i < n &&
            !isSpace(input[i]) &&
            input[i] !== ',' &&
            input[i] !== '"' &&
            input[i] !== '(' &&
            input[i] !== ')'
          ) {
            i++;
          }
          if (i < n && (input[i] === '(' || input[i] === ')')) {
            return {
              error: {
                code: 'syntax',
                message: 'parentheses are not supported',
                offset: i,
                length: 1,
              },
            };
          }
          if (i < n && input[i] === '"') {
            return {
              error: {
                code: 'syntax',
                message: 'unexpected quote in unquoted value',
                offset: i,
                length: 1,
              },
            };
          }
          values.push(input.slice(valStart, i));
        }

        if (values.length > MAX_VALUES_PER_KEY) {
          return {
            error: {
              code: 'limit',
              message: `key "${lowerKey}" exceeds maximum of ${MAX_VALUES_PER_KEY} values`,
              offset: termStart,
              length: i - termStart,
            },
          };
        }

        if (i < n && input[i] === ',') {
          i++;
          continue;
        }
        break;
      }

      terms.push({
        key: lowerKey,
        values,
        negate,
        offset: termStart,
        length: i - termStart,
      });
      continue;
    }

    // Bareword text term (including unknown key:value tokens).
    const wordStart = i;
    while (i < n && !isSpace(input[i])) {
      if (input[i] === '(' || input[i] === ')') {
        return {
          error: {
            code: 'syntax',
            message: 'parentheses are not supported',
            offset: i,
            length: 1,
          },
        };
      }
      i++;
    }
    const word = input.slice(wordStart, i);
    if (!negate && (word === 'OR' || word === 'AND')) {
      return {
        error: {
          code: 'syntax',
          message: 'use key:a,b for alternatives; boolean operators OR and AND are not supported',
          offset: termStart,
          length: word.length,
        },
      };
    }
    if (negate) {
      excludeText.push(word);
    } else {
      posTextTokens.push(word);
    }
  }

  if (terms.length + excludeText.length > MAX_TERMS) {
    return {
      error: {
        code: 'limit',
        message: `filter query exceeds maximum of ${MAX_TERMS} terms`,
        offset: 0,
        length: input.length,
      },
    };
  }

  const text = posTextTokens.join(' ');
  if (runeCount(text) > MAX_TEXT_RUNES) {
    return {
      error: {
        code: 'limit',
        message: `search text exceeds maximum length of ${MAX_TEXT_RUNES} characters`,
        offset: 0,
        length: input.length,
      },
    };
  }
  for (const ex of excludeText) {
    if (runeCount(ex) > MAX_TEXT_RUNES) {
      return {
        error: {
          code: 'limit',
          message: `excluded text exceeds maximum length of ${MAX_TEXT_RUNES} characters`,
          offset: 0,
          length: input.length,
        },
      };
    }
  }

  const query: {
    mode: 'structured';
    terms: Term[];
    text?: string;
    excludeText?: string[];
  } = {
    mode: 'structured',
    terms,
  };
  if (text !== '') query.text = text;
  if (excludeText.length > 0) query.excludeText = excludeText;
  return { query };
}
