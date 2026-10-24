// SPDX-License-Identifier: AGPL-3.0-only

// Small strict decoders for the API's JSON. Each one checks the shape and
// copies only the known fields, so the result has exactly the fields the
// client knows about. The contract test (api.contract.test.ts) decodes
// the Go side's golden files and compares the result with the file, so a
// field added, renamed or dropped on either side fails a test (Q9).

export class DecodeError extends Error {
  constructor(path: string, want: string) {
    super(`${path}: expected ${want}`)
    this.name = 'DecodeError'
  }
}

export type Decoder<T> = (v: unknown, path: string) => T

export const str: Decoder<string> = (v, path) => {
  if (typeof v !== 'string') throw new DecodeError(path, 'a string')
  return v
}

export const bool: Decoder<boolean> = (v, path) => {
  if (typeof v !== 'boolean') throw new DecodeError(path, 'a boolean')
  return v
}

export const int: Decoder<number> = (v, path) => {
  if (typeof v !== 'number' || !Number.isSafeInteger(v)) throw new DecodeError(path, 'an integer')
  return v
}

export function nullable<T>(d: Decoder<T>): Decoder<T | null> {
  return (v, path) => (v === null ? null : d(v, path))
}

export function array<T>(d: Decoder<T>): Decoder<T[]> {
  return (v, path) => {
    if (!Array.isArray(v)) throw new DecodeError(path, 'an array')
    return v.map((x, i) => d(x, `${path}[${i}]`))
  }
}

export function oneOf<T extends string>(values: readonly T[]): Decoder<T> {
  return (v, path) => {
    if (typeof v !== 'string' || !(values as readonly string[]).includes(v)) {
      throw new DecodeError(path, `one of ${values.join(', ')}`)
    }
    return v as T
  }
}

// A timestamp stays a string (RFC 3339, as Go sends it); it must parse.
export const time: Decoder<string> = (v, path) => {
  const s = str(v, path)
  if (Number.isNaN(Date.parse(s))) throw new DecodeError(path, 'an RFC 3339 time')
  return s
}

type Fields<T> = { [K in keyof T]-?: Decoder<T[K]> }

// object decodes the fields in required, and the fields in optional when
// present (Go's omitempty). Unknown fields are dropped.
export function object<T extends object, O extends object = Record<never, never>>(
  required: Fields<T>,
  optional?: Fields<O>,
): Decoder<T & Partial<O>> {
  return (v, path) => {
    if (typeof v !== 'object' || v === null || Array.isArray(v)) throw new DecodeError(path, 'an object')
    const o = v as Record<string, unknown>
    const out: Record<string, unknown> = {}
    for (const [k, d] of Object.entries(required) as [string, Decoder<unknown>][]) {
      if (!(k in o)) throw new DecodeError(`${path}.${k}`, 'a field')
      out[k] = d(o[k], `${path}.${k}`)
    }
    if (optional) {
      for (const [k, d] of Object.entries(optional) as [string, Decoder<unknown>][]) {
        if (k in o) out[k] = d(o[k], `${path}.${k}`)
      }
    }
    return out as T & Partial<O>
  }
}
