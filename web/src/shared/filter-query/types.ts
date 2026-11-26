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

export const MAX_INPUT_BYTES = 512;
export const MAX_TERMS = 32;
export const MAX_VALUES_PER_KEY = 20;
export const MAX_TEXT_RUNES = 200;
export const MAX_DISTINCT_REFERENTS = 20;

export type FilterErrorCode = 'syntax' | 'value' | 'limit' | 'unsupported_key';

export interface FilterError {
  code: FilterErrorCode;
  message: string;
  offset: number;
  length: number;
}

export interface FilterWarning {
  code: 'unknown_key' | 'repeated_key';
  message: string;
  offset: number;
  length: number;
}

export interface Term {
  key: string;
  values: string[];
  negate: boolean;
  offset: number;
  length: number;
}

export type Query =
  | { mode: 'text'; text: string }
  | { mode: 'structured'; terms: Term[]; text?: string; excludeText?: string[] };

export type ExecClass = 'server' | 'client';
export type KeyKind = 'enum' | 'string' | 'label' | 'flag_set' | 'ref';

export interface KeySchema {
  name: string;
  help: string;
  kind: KeyKind;
  exec: ExecClass;
  enum?: string[];
  exclusive?: string[][];
  multiValued: boolean;
}

export interface ViewSchema {
  view: string;
  keys: KeySchema[];
  textHelp: string;
}

export interface FilterRow {
  values(key: string): string[];
  text(): string[];
}

export interface ParseResult {
  query?: Query;
  error?: FilterError;
}

export interface ValidationResult {
  error?: FilterError;
  warnings: FilterWarning[];
}
