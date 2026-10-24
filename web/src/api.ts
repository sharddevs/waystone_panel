// SPDX-License-Identifier: AGPL-3.0-only

// The typed client for the browser API on web.sock (stage 5b) and the
// session routes (5a-2). The shapes mirror internal/view; the decoders
// keep them honest (Q9, api.contract.test.ts).

import { array, bool, int, nullable, object, oneOf, str, time, type Decoder } from './decode'
import { densities, modes, themes, type Preferences } from './appearance'

// ---- Shapes (internal/view) ----

export const processStates = ['stopped', 'starting', 'running', 'stopping', 'failed', 'unknown'] as const
export const readinessStates = ['ready', 'not_ready', 'unknown', 'not_supported'] as const
export const operationStates = ['pending', 'running', 'succeeded', 'failed', 'uncertain'] as const
// serverActions have buttons on the server page; listedActions are every
// action the API lists (install and accept_eula since 6c, command since
// 7b).
export const serverActions = ['start', 'stop', 'restart'] as const
export const listedActions = [...serverActions, 'install', 'accept_eula', 'command'] as const

export type ProcessState = (typeof processStates)[number]
export type ReadinessState = (typeof readinessStates)[number]
export type OperationState = (typeof operationStates)[number]
export type ServerAction = (typeof serverActions)[number]
export type ListedAction = (typeof listedActions)[number]

export function isServerAction(a: string): a is ServerAction {
  return (serverActions as readonly string[]).includes(a)
}

// PageAction is anything the server page can ask for with an empty body:
// the buttons, the install, and the EULA (6d). A command carries its line
// (client.command).
export type PageAction = Exclude<ListedAction, 'command'>

const actionVerbs: Record<PageAction, string> = {
  start: 'start',
  stop: 'stop',
  restart: 'restart',
  install: 'install',
  accept_eula: 'accept-eula',
}

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
  // command is a console command's line (7b), on server.command only.
  command?: string
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
  port: number | null
  heap_mib: number | null
  installed: boolean
  eula_accepted: boolean
  actions: { action: ListedAction; allowed: boolean; reason?: string }[]
}

// NodeDetail is a node on the Nodes page (6b, view.NodeDetail).
export interface NodeDetail {
  id: string
  name: string
  reachability: { state: 'reachable' | 'unreachable'; last_seen_at: string | null }
  link: 'local' | 'mtls'
  agent_version: string | null
  capabilities: string[]
  enrolled_at: string | null
  revoked_at: string | null
  server_count: number
  actions: { action: 'revoke'; allowed: boolean; reason?: string }[]
}

export const enrollmentStates = ['pending', 'used', 'expired'] as const

export interface Enrollment {
  id: string
  name: string
  created_by: Actor
  created_at: string
  expires_at: string
  used_at: string | null
  node_id: string | null
  state: (typeof enrollmentStates)[number]
}

// NewEnrollment is the one answer that carries the bundle (D2).
export interface NewEnrollment {
  enrollment: Enrollment
  bundle: string
  command: string
}

export interface NewServer {
  node_id: string
  name: string
  port: number
  heap_mib: number
}

export interface CreatedServer {
  server: Server
  operation: Operation
}

// LogLine and LogPage are one read of a server's log (7a), oldest line
// first. cursor is where the next read continues, null before the
// server has logged anything; more says lines already follow it.
export interface LogLine {
  time: string
  priority: number
  text: string
}

export interface LogPage {
  lines: LogLine[]
  cursor: string | null
  more: boolean
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
  { phase: str, error_code: str, error_summary: str, command: str },
)

const reachability = object({ state: oneOf(['reachable', 'unreachable'] as const), last_seen_at: nullable(time) })

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
    reachability,
  }),
  profile: object({ id: str, version: nullable(str) }),
  runtime: str,
  desired_state: oneOf(['running', 'stopped'] as const),
  process: dimension(processStates),
  readiness: dimension(readinessStates),
  port: nullable(int),
  heap_mib: nullable(int),
  installed: bool,
  eula_accepted: bool,
  actions: array(object({ action: oneOf(listedActions), allowed: bool }, { reason: str })),
})

export const decodeServerList: Decoder<{ servers: Server[] }> = object({ servers: array(decodeServer) })
export const decodeOperationList: Decoder<{ operations: Operation[] }> = object({ operations: array(decodeOperation) })

export const decodeNode: Decoder<NodeDetail> = object({
  id: str,
  name: str,
  reachability,
  link: oneOf(['local', 'mtls'] as const),
  agent_version: nullable(str),
  capabilities: array(str),
  enrolled_at: nullable(time),
  revoked_at: nullable(time),
  server_count: int,
  actions: array(object({ action: oneOf(['revoke'] as const), allowed: bool }, { reason: str })),
})

export const decodeNodeList: Decoder<{ nodes: NodeDetail[] }> = object({ nodes: array(decodeNode) })

export const decodeEnrollment: Decoder<Enrollment> = object({
  id: str,
  name: str,
  created_by: actor,
  created_at: time,
  expires_at: time,
  used_at: nullable(time),
  node_id: nullable(str),
  state: oneOf(enrollmentStates),
})

export const decodeEnrollmentList: Decoder<{ enrollments: Enrollment[] }> = object({ enrollments: array(decodeEnrollment) })
export const decodeNewEnrollment: Decoder<NewEnrollment> = object({ enrollment: decodeEnrollment, bundle: str, command: str })
export const decodeCreatedServer: Decoder<CreatedServer> = object({ server: decodeServer, operation: decodeOperation })

export const decodeLogPage: Decoder<LogPage> = object({
  lines: array(object({ time, priority: int, text: str })),
  cursor: nullable(str),
  more: bool,
})

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
  act(serverId: string, action: PageAction, csrf: string, idempotencyKey: string): Promise<Operation> {
    return this.request('POST', `/api/v1/servers/${encodeURIComponent(serverId)}/${actionVerbs[action]}`, decodeOperation, {
      body: {},
      csrf,
      idempotencyKey,
    })
  }
  // command sends one line to the server's console (7b). The answer is
  // the operation; the line is written when the node runs it.
  command(serverId: string, line: string, csrf: string, idempotencyKey: string): Promise<Operation> {
    return this.request('POST', `/api/v1/servers/${encodeURIComponent(serverId)}/command`, decodeOperation, {
      body: { command: line },
      csrf,
      idempotencyKey,
    })
  }
  // createServer answers with the server and its first install
  // operation; the same key and body again is the same answer (6c).
  createServer(body: NewServer, csrf: string, idempotencyKey: string): Promise<CreatedServer> {
    return this.request('POST', '/api/v1/servers', decodeCreatedServer, { body, csrf, idempotencyKey })
  }

  // logs reads a server's log (7a): the newest lines, or with after the
  // lines after that cursor. Admins only.
  logs(serverId: string, after: string | null = null, limit?: number): Promise<LogPage> {
    const p = new URLSearchParams()
    if (after !== null) p.set('after', after)
    if (limit !== undefined) p.set('limit', String(limit))
    const qs = p.toString()
    return this.request('GET', `/api/v1/servers/${encodeURIComponent(serverId)}/logs${qs ? `?${qs}` : ''}`, decodeLogPage)
  }

  // Nodes (6b). Admins only; anyone else gets 403 forbidden.
  nodes(): Promise<{ nodes: NodeDetail[] }> {
    return this.request('GET', '/api/v1/nodes', decodeNodeList)
  }
  revokeNode(id: string, csrf: string): Promise<NodeDetail> {
    return this.request('POST', `/api/v1/nodes/${encodeURIComponent(id)}/revoke`, decodeNode, { body: {}, csrf })
  }
  enrollments(): Promise<{ enrollments: Enrollment[] }> {
    return this.request('GET', '/api/v1/nodes/enrollments', decodeEnrollmentList)
  }
  createEnrollment(name: string, csrf: string): Promise<NewEnrollment> {
    return this.request('POST', '/api/v1/nodes/enrollments', decodeNewEnrollment, { body: { name }, csrf })
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
