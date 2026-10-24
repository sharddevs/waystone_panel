// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from 'node:fs'
import { vi } from 'vitest'
import { Client } from '../api'

export interface Call {
  method: string
  path: string
  headers: Record<string, string>
  body: unknown
  init: RequestInit
}

export type Reply = { status: number; body?: unknown; headers?: Record<string, string> }

// fakeClient is a Client over a fetch that answers from handler and
// records every call.
export function fakeClient(handler: (c: Call) => Reply | Promise<Reply>) {
  const calls: Call[] = []
  const fetchFn = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const c: Call = {
      method: init.method ?? 'GET',
      path: String(input),
      headers: (init.headers ?? {}) as Record<string, string>,
      body: typeof init.body === 'string' ? JSON.parse(init.body) : undefined,
      init,
    }
    calls.push(c)
    const r = await handler(c)
    return new Response(r.body === undefined ? null : JSON.stringify(r.body), {
      status: r.status,
      headers: { 'Content-Type': 'application/json', ...r.headers },
    })
  })
  return { client: new Client(fetchFn as unknown as typeof fetch), calls, fetchFn }
}

export const goldenSession = JSON.parse(readFileSync('../internal/session/testdata/session.json', 'utf8')) as {
  csrf_token: string
  user: { display_name: string }
}

export function webError(code: string, status: number, message = 'x'): Reply {
  return { status, body: { error: { code, message } } }
}
