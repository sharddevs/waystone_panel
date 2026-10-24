// SPDX-License-Identifier: AGPL-3.0-only

// The typed client for the browser API on web.sock (stage 5b) and the
// session routes (5a-2). The shapes mirror internal/view; the decoders
// keep them honest (Q9, api.contract.test.ts).

import { array, bool, nullable, object, oneOf, str, time, type Decoder } from './decode'
import { densities, modes, themes, type Preferences } from './appearance'

// ---- Shapes (internal/view) ----

export const processStates = ['stopped', 'starting', 'running', 'stopping', 'failed', 'unknown'] as const
export const readinessStates = ['ready', 'not_ready', 'unknown', 'not_supported'] as const
export const operationStates = ['pending', 'running', 'succeeded', 'failed', 'uncertain'] as const
export const serverActions = ['start', 'stop', 'restart'] as const

export type ProcessState = (typeof processStates)[number]
export type ReadinessState = (typeof readinessStates)[number]
export type OperationState = (typeof operationStates)[number]
export type ServerAction = (typeof serverActions)[number]

export interface Actor {
  type: string
  id: string
  name: string
}

export interface Resolution {
  at: string
  outcome: 'succeeded' | 'failed'
  by: Actor
  note: string
}

export interface Operation {
  id: string
  server_id: string
  node_id: string
  action: string
  state: OperationState
  phase?: string
  actor: Actor
  error_code?: string
  error_summary?: string
  requested_at: string
  started_at: string | null
  finished_at: string | null
  resolution: Resolution | null
}

export interface Dimension<S extends string> {
  state: S
  since: string | null
  observed_at: string | null
  last_reported?: S
}

export interface Server {
  id: string
  name: string
  node: {
    id: string
    name: string
    reachability: { state: 'reachable' | 'unreachable'; last_seen_at: string | null }
  }
  profile: { id: string; version: string | null }
  runtime: string
  desired_state: 'running' | 'stopped'
  process: Dimension<ProcessState>
  readiness: Dimension<ReadinessState>
  actions: { action: ServerAction; allowed: boolean; reason?: string }[]
}

export interface Session {
  user: { id: string; display_name: string }
  csrf_token: string
  expires_at: string
  preferences: Preferences
}

// ---- Decoders ----

const actor: Decoder<Actor> = object({ type: str, id: str, name: str })

const resolution: Decoder<Resolution> = object({
  at: time,
  outcome: oneOf(['succeeded', 'failed'] as const),
  by: actor,
  note: str,
})

export const decodeOperation: Decoder<Operation> = object(
  {
    id: str,
    server_id: str,
    node_id: str,
    action: str,
    state: oneOf(operationStates),
    actor,
    requested_at: time,
    started_at: nullable(time),
    finished_at: nullable(time),
    resolution: nullable(resolution),
  },
  { phase: str, error_code: str, error_summary: str },
)

function dimension<S extends string>(states: readonly S[]): Decoder<Dimension<S>> {
  return object<{ state: S; since: string | null; observed_at: string | null }, { last_reported: S }>(
    { state: oneOf(states), since: nullable(time), observed_at: nullable(time) },
    { last_reported: oneOf(states) },
  )
}

export const decodeServer: Decoder<Server> = object({
  id: str,
  name: str,
  node: object({
    id: str,
    name: str,
    reachability: object({ state: oneOf(['reachable', 'unreachable'] as const), last_seen_at: nullable(time) }),
  }),
  profile: object({ id: str, version: nullable(str) }),
  runtime: str,
  desired_state: oneOf(['running', 'stopped'] as const),
  process: dimension(processStates),
  readiness: dimension(readinessStates),
  actions: array(object({ action: oneOf(serverActions), allowed: bool }, { reason: str })),
})

export const decodeServerList: Decoder<{ servers: Server[] }> = object({ servers: array(decodeServer) })
export const decodeOperationList: Decoder<{ operations: Operation[] }> = object({ operations: array(decodeOperation) })

export const decodePreferences: Decoder<Preferences> = object({
  theme: oneOf(themes),
  density: oneOf(densities),
  mode: oneOf(modes),
})

export const decodeSession: Decoder<Session> = object({
  user: object({ id: str, display_name: str }),
  csrf_token: str,
  expires_at: time,
  preferences: decodePreferences,
})

// Both error shapes: the service's (code, message, request_id, maybe
// operation_id) and web's own refusals (code, message).
export interface ErrorBody {
  error: { code: string; message: string; request_id?: string; operation_id?: string }
}

export const decodeErrorBody: Decoder<ErrorBody> = object({
  error: object({ code: str, message: str }, { request_id: str, operation_id: str }),
})

// ---- Errors ----

// ApiError is any answer other than the expected success. code is the
// API's error code, or "network" / "bad_response" when there was no
// usable answer at all.
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId: string | undefined
  readonly operationId: string | undefined
  // retryAfter is the Retry-After header in seconds, when sent.
  readonly retryAfter: number | undefined

  constructor(status: number, code: string, message: string, extra: { requestId?: string | undefined; operationId?: string | undefined; retryAfter?: number | undefined } = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = extra.requestId
    this.operationId = extra.operationId
    this.retryAfter = extra.retryAfter
  }
}

// parseRetryAfter reads a Retry-After header given in seconds.
export function parseRetryAfter(h: string | null): number | undefined {
  if (h === null || !/^\d{1,6}$/.test(h)) return undefined
  return Number(h)
}

// ---- The client ----

export interface RequestOptions {
  body?: unknown
  csrf?: string
  idempotencyKey?: string
}

export type Fetch = typeof fetch

export class Client {
  private readonly fetchFn: Fetch

  constructor(fetchFn: Fetch = (...a) => globalThis.fetch(...a)) {
    this.fetchFn = fetchFn
  }

  // request sends one request and returns the decoded body; for 204 it
  // returns undefined (decode is not called).
  async request<T>(method: string, path: string, decode: Decoder<T> | null, opts: RequestOptions = {}): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' }
    let body: string | undefined
    if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(opts.body)
    }
    if (opts.csrf !== undefined) headers['X-CSRF-Token'] = opts.csrf
    if (opts.idempotencyKey !== undefined) headers['Idempotency-Key'] = opts.idempotencyKey

    let res: Response
    try {
      const init: RequestInit = { method, headers, credentials: 'same-origin', cache: 'no-store', redirect: 'error' }
      if (body !== undefined) init.body = body
      res = await this.fetchFn(path, init)
    } catch {
      throw new ApiError(0, 'network', "Couldn't reach Waystone.")
    }
    if (res.ok) {
      if (res.status === 204 || decode === null) return undefined as T
      let json: unknown
      try {
        json = await res.json()
        return decode(json, 'response')
      } catch {
        throw new ApiError(res.status, 'bad_response', 'Waystone sent an answer this page does not understand.')
      }
    }
    const retryAfter = parseRetryAfter(res.headers.get('Retry-After'))
    try {
      const e = decodeErrorBody(await res.json(), 'error').error
      throw new ApiError(res.status, e.code, e.message, { requestId: e.request_id, operationId: e.operation_id, retryAfter })
    } catch (err) {
      if (err instanceof ApiError) throw err
      throw new ApiError(res.status, 'bad_response', `Waystone answered ${res.status}.`, { retryAfter })
    }
  }

  // Session (5a-2).
  signIn(username: string, password: string): Promise<void> {
    return this.request('POST', '/api/v1/session', null, { body: { username, password } })
  }
  session(): Promise<Session> {
    return this.request('GET', '/api/v1/session', decodeSession)
  }
  signOut(csrf: string): Promise<void> {
    return this.request('DELETE', '/api/v1/session', null, { csrf })
  }
  setPreferences(p: Preferences, csrf: string): Promise<Preferences> {
    return this.request('PATCH', '/api/v1/session/preferences', decodePreferences, { body: p, csrf })
  }

  // Servers and operations (5b).
  servers(): Promise<{ servers: Server[] }> {
    return this.request('GET', '/api/v1/servers', decodeServerList)
  }
  server(id: string): Promise<Server> {
    return this.request('GET', `/api/v1/servers/${encodeURIComponent(id)}`, decodeServer)
  }
  operations(q: { serverId?: string; limit?: number } = {}): Promise<{ operations: Operation[] }> {
    const p = new URLSearchParams()
    if (q.serverId !== undefined) p.set('server_id', q.serverId)
    if (q.limit !== undefined) p.set('limit', String(q.limit))
    const qs = p.toString()
    return this.request('GET', `/api/v1/operations${qs ? `?${qs}` : ''}`, decodeOperationList)
  }
  operation(id: string): Promise<Operation> {
    return this.request('GET', `/api/v1/operations/${encodeURIComponent(id)}`, decodeOperation)
  }
  act(serverId: string, action: ServerAction, csrf: string, idempotencyKey: string): Promise<Operation> {
    return this.request('POST', `/api/v1/servers/${encodeURIComponent(serverId)}/${action}`, decodeOperation, {
      body: {},
      csrf,
      idempotencyKey,
    })
  }
  resolve(operationId: string, outcome: 'succeeded' | 'failed', note: string, csrf: string, idempotencyKey: string): Promise<Operation> {
    return this.request('POST', `/api/v1/operations/${encodeURIComponent(operationId)}/resolve`, decodeOperation, {
      body: { outcome, note },
      csrf,
      idempotencyKey,
    })
  }
}

export const client = new Client()
