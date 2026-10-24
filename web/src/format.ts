// SPDX-License-Identifier: AGPL-3.0-only

// Words for operations and API errors (server page §4, wireframes §3-§4,
// api requirements C1-C7). Everything a person reads about an operation
// comes from here, so the list, the active line and the details page say
// the same thing.

import { ApiError, type Operation, type OperationState, type Server } from './api'
import type { MarkSymbol } from './components/StatusMark'
import { formatDuration, type Tone } from './status'

const actionNames: Record<string, string> = {
  'server.start': 'Start server',
  'server.stop': 'Stop server',
  'server.restart': 'Restart server',
  'server.install': 'Install server',
  'server.accept_eula': 'Accept EULA',
}

// actionName is "Start server" for server.start. An action this page
// doesn't know yet is shown as it is, never hidden.
export function actionName(action: string): string {
  return actionNames[action] ?? action
}

// actionVerb is how an uncertain outcome is explained: "whether the
// server started".
function actionPast(action: string): string {
  switch (action) {
    case 'server.start':
      return 'started'
    case 'server.stop':
      return 'stopped'
    case 'server.restart':
      return 'restarted'
    case 'server.install':
      return 'was installed'
    case 'server.accept_eula':
      return 'had its EULA accepted'
    default:
      return 'did what was asked'
  }
}

export function uncertainText(action: string): string {
  return `Waystone lost contact while this was running, so it doesn't know whether the server ${actionPast(action)}.`
}

export const stateWords: Record<OperationState, string> = {
  pending: 'Pending',
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
  uncertain: 'Outcome uncertain',
}

// stateMark is the operation state's symbol and colour (design tokens,
// "Status marks"): uncertain is the question mark, never a cross or a
// check.
export function stateMark(s: OperationState): { symbol: MarkSymbol; tone: Tone } {
  switch (s) {
    case 'pending':
    case 'running':
      return { symbol: 'transition', tone: 'warn' }
    case 'succeeded':
      return { symbol: 'succeeded', tone: 'ok' }
    case 'failed':
      return { symbol: 'failed', tone: 'bad' }
    case 'uncertain':
      return { symbol: 'unknown', tone: 'unk' }
  }
}

export function isActive(o: Operation): boolean {
  return o.state === 'pending' || o.state === 'running'
}

function ms(t: string | null): number | undefined {
  if (t === null) return undefined
  const v = Date.parse(t)
  return Number.isNaN(v) ? undefined : v
}

// took is how long a finished operation ran ("3.2 s"), or how long an
// active one has been going ("4 s…"). Without the timestamps it needs it
// is undefined: a duration is never made up (§2).
export function took(o: Operation, now: Date): string | undefined {
  const start = ms(o.started_at) ?? (isActive(o) ? ms(o.requested_at) : undefined)
  if (start === undefined) return undefined
  if (isActive(o)) return `${formatDuration(now.getTime() - start)}…`
  const end = ms(o.finished_at)
  if (end === undefined) return undefined
  const d = Math.max(0, end - start)
  return d < 60_000 ? `${(d / 1000).toFixed(1)} s` : formatDuration(d)
}

// outcome is the last column of a history row: the error summary for a
// failure, else the duration.
export function outcome(o: Operation, now: Date): string {
  if (o.state === 'failed' && o.error_summary) return o.error_summary
  return took(o, now) ?? ''
}

// formatTime is a wall-clock time in the viewer's own locale and time
// zone: the time alone for today, with the date otherwise.
export function formatTime(t: string | null, now: Date, seconds = false): string {
  const v = ms(t)
  if (v === undefined) return '—'
  const d = new Date(v)
  const time = d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit', ...(seconds ? { second: '2-digit' } : {}) })
  if (d.toDateString() === now.toDateString()) return time
  const date = d.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
    ...(d.getFullYear() !== now.getFullYear() ? { year: 'numeric' } : {}),
  })
  return `${date}, ${time}`
}

// ago is "8 s ago", or undefined for an unknown time.
export function ago(t: string | null, now: Date): string | undefined {
  const v = ms(t)
  return v === undefined ? undefined : `${formatDuration(now.getTime() - v)} ago`
}

// errorText is what a person reads for a refused or failed request. The
// request ID, when there is one, is shown separately and small (A5).
export function errorText(e: unknown): string {
  if (!(e instanceof ApiError)) return 'Something went wrong.'
  switch (e.code) {
    case 'network':
      return "Couldn't reach Waystone."
    case 'node_unreachable':
      return "The node is unreachable, so Waystone can't do this now."
    case 'operation_in_progress':
      return 'Another operation on this server is still in progress.'
    case 'previous_operation_uncertain':
      return "The last operation's outcome is uncertain. An admin has to resolve it first."
    case 'forbidden':
      return "You don't have permission to do this."
    case 'rate_limited':
    case 'too_many_requests':
      return e.retryAfter === undefined ? 'Too many requests. Try again shortly.' : `Too many requests. Try again in ${formatDuration(e.retryAfter * 1000)}.`
    case 'controller_busy':
      return 'Waystone is busy. Try again in a moment.'
    case 'not_found':
      return 'It no longer exists, or you no longer have access to it.'
    case 'already_resolved':
      return 'Someone already resolved this operation.'
    case 'not_uncertain':
      return "This operation's outcome isn't uncertain."
    case 'node_revoked':
      return 'The node was revoked, so Waystone can no longer manage its servers.'
    case 'server_running':
      return 'The server is running. Stop it first.'
    case 'not_installed':
      return "The server isn't installed yet."
    case 'eula_not_accepted':
      return "The Minecraft EULA hasn't been accepted for this server yet."
    case 'already_accepted':
      return 'The EULA was already accepted for this server.'
    case 'port_taken':
      return 'Another server on this node already uses that port.'
    case 'node_full':
      return 'This node already has as many servers as it can manage.'
    case 'enrollment_unavailable':
      return "Waystone isn't listening for other machines, so it can't add a node yet."
    case 'bad_request':
      return 'Waystone refused the request as it was sent. Check the fields and try again.'
    case 'csrf_failed':
      return 'Your page is out of date. Reload it and try again.'
    case 'bad_response':
      return 'Waystone sent an answer this page does not understand.'
    default:
      return `Something went wrong (${e.code}).`
  }
}

const gameNames: Record<string, string> = { 'minecraft-java': 'Minecraft Java' }

// gameLine is "Minecraft Java 26.3". The version is left out when it is
// unknown (V3); a profile this page doesn't know shows its id.
export function gameLine(s: Server): string {
  const game = gameNames[s.profile.id] ?? s.profile.id
  return s.profile.version ? `${game} ${s.profile.version}` : game
}
