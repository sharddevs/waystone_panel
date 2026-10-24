// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { ApiError, type Operation, type Server } from './api'
import { actionName, ago, errorText, formatTime, gameLine, isActive, outcome, stateMark, stateWords, took, uncertainText } from './format'
import { dimensionsLine } from './status'

const ops = (JSON.parse(readFileSync('../internal/view/testdata/operations.json', 'utf8')) as { operations: Operation[] }).operations
const [running, failed, uncertain] = ops as [Operation, Operation, Operation]
const server = JSON.parse(readFileSync('../internal/view/testdata/server.json', 'utf8')) as Server
const now = new Date('2026-10-16T12:00:00Z')

describe('actionName', () => {
  it.each([
    ['server.start', 'Start server'],
    ['server.stop', 'Stop server'],
    ['server.restart', 'Restart server'],
    ['server.backup', 'server.backup'],
  ])('%s → %s', (a, want) => expect(actionName(a)).toBe(want))
})

it('uncertain explains what Waystone does not know', () => {
  expect(uncertainText('server.stop')).toBe("Waystone lost contact while this was running, so it doesn't know whether the server stopped.")
  expect(uncertainText('server.restart')).toContain('whether the server restarted')
})

it('uncertain is its own state: words and mark are never failed or succeeded', () => {
  expect(stateWords.uncertain).toBe('Outcome uncertain')
  expect(stateMark('uncertain')).toEqual({ symbol: 'unknown', tone: 'unk' })
  expect(stateMark('failed')).toEqual({ symbol: 'failed', tone: 'bad' })
  expect(stateMark('succeeded')).toEqual({ symbol: 'succeeded', tone: 'ok' })
  expect(stateMark('pending')).toEqual({ symbol: 'transition', tone: 'warn' })
  expect(stateMark('running')).toEqual({ symbol: 'transition', tone: 'warn' })
})

describe('took and outcome', () => {
  it('a finished operation: its real duration', () => {
    expect(took(failed, now)).toBe('7.1 s')
    expect(took({ ...failed, finished_at: '2026-10-16T11:35:02Z' }, now)).toBe('5 min')
  })
  it('an active one: elapsed, marked as still going', () => {
    expect(isActive(running)).toBe(true)
    expect(took(running, now)).toBe('9 min…')
    // Pending, not started yet: elapsed since it was requested.
    expect(took({ ...running, state: 'pending', started_at: null, requested_at: '2026-10-16T11:59:56Z' }, now)).toBe('4 s…')
  })
  it('never makes a duration up', () => {
    expect(took(uncertain, now)).toBeUndefined()
    expect(took({ ...failed, started_at: null }, now)).toBeUndefined()
    expect(outcome(uncertain, now)).toBe('')
  })
  it('a failure shows its summary instead of a duration', () => {
    expect(outcome(failed, now)).toBe('systemd was busy with this server.')
    expect(outcome({ ...failed, state: 'succeeded' }, now)).toBe('7.1 s')
  })
})

describe('formatTime and ago', () => {
  it('today: just the time; another day: the date too', () => {
    const today = formatTime('2026-10-16T11:30:02Z', now)
    expect(today).toBe(new Date('2026-10-16T11:30:02Z').toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' }))
    expect(formatTime('2026-10-14T11:30:02Z', now)).toContain(', ')
    expect(formatTime('2025-10-14T11:30:02Z', now)).toContain('2025')
    expect(formatTime('2026-10-16T11:30:02Z', now, true)).toContain('02')
  })
  it('an unknown time is a dash, never a made-up one', () => {
    expect(formatTime(null, now)).toBe('—')
    expect(formatTime('garbage', now)).toBe('—')
    expect(ago(null, now)).toBeUndefined()
    expect(ago('2026-10-16T11:59:52Z', now)).toBe('8 s ago')
  })
})

describe('errorText', () => {
  it.each([
    ['network', 0, "Couldn't reach Waystone."],
    ['node_unreachable', 409, "The node is unreachable, so Waystone can't do this now."],
    ['operation_in_progress', 409, 'Another operation on this server is still in progress.'],
    ['previous_operation_uncertain', 409, "The last operation's outcome is uncertain. An admin has to resolve it first."],
    ['forbidden', 403, "You don't have permission to do this."],
    ['controller_busy', 503, 'Waystone is busy. Try again in a moment.'],
    ['already_resolved', 409, 'Someone already resolved this operation.'],
    ['csrf_failed', 403, 'Your page is out of date. Reload it and try again.'],
    ['something_new', 500, 'Something went wrong (something_new).'],
  ])('%s', (code, status, want) => {
    expect(errorText(new ApiError(status, code, 'server text is never shown'))).toBe(want)
  })
  it('rate limited says when, if it knows', () => {
    expect(errorText(new ApiError(429, 'rate_limited', 'x', { retryAfter: 30 }))).toBe('Too many requests. Try again in 30 s.')
    expect(errorText(new ApiError(429, 'rate_limited', 'x'))).toBe('Too many requests. Try again shortly.')
  })
  it('anything that is not an ApiError', () => {
    expect(errorText(new Error('boom'))).toBe('Something went wrong.')
  })
})

it('gameLine leaves out an unknown version (V3)', () => {
  expect(gameLine(server)).toBe('Minecraft Java 26.3')
  expect(gameLine({ ...server, profile: { id: 'minecraft-java', version: null } })).toBe('Minecraft Java')
  expect(gameLine({ ...server, profile: { id: 'valheim', version: '1' } })).toBe('valheim 1')
})

describe('dimensionsLine', () => {
  it('all three dimensions in words', () => {
    expect(dimensionsLine(server, now)).toBe('Node reachable · Process running for 12 min · Game ready')
  })
  it('no duration without a since', () => {
    expect(dimensionsLine({ ...server, process: { ...server.process, since: null } }, now)).toBe('Node reachable · Process running · Game ready')
  })
  it('the other states', () => {
    const s = { ...server, node: { ...server.node, reachability: { state: 'unreachable' as const, last_seen_at: null } } }
    expect(dimensionsLine({ ...s, process: { ...s.process, state: 'unknown' }, readiness: { ...s.readiness, state: 'unknown' } }, now)).toBe(
      'Node unreachable · Process state unknown · Game readiness unknown',
    )
    expect(dimensionsLine({ ...server, process: { ...server.process, state: 'stopped' }, readiness: { ...server.readiness, state: 'not_ready' } }, now)).toBe(
      'Node reachable · Process stopped · Game not ready',
    )
    expect(dimensionsLine({ ...server, readiness: { ...server.readiness, state: 'not_supported' } }, now)).toBe(
      'Node reachable · Process running for 12 min · No readiness check',
    )
  })
})
