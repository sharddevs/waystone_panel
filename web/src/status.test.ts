// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from 'vitest'
import type { ProcessState, ReadinessState, Server } from './api'
import { formatDuration, reasonText, summarize } from './status'

const now = new Date('2026-10-16T12:00:00Z')
const ago = (s: number) => new Date(now.getTime() - s * 1000).toISOString()

function server(o: {
  reachable?: boolean
  process: ProcessState
  readiness?: ReadinessState
  desired?: 'running' | 'stopped'
  since?: string | null
  observed?: string | null
  lastReported?: ProcessState
  profile?: string
}): Server {
  const process: Server['process'] = { state: o.process, since: o.since ?? null, observed_at: o.observed === undefined ? ago(8) : o.observed }
  if (o.lastReported !== undefined) process.last_reported = o.lastReported
  return {
    id: 's',
    name: 'Survival World',
    node: { id: 'n', name: 'homelab-1', reachability: { state: o.reachable === false ? 'unreachable' : 'reachable', last_seen_at: ago(5) } },
    profile: { id: o.profile ?? 'minecraft-java', version: null },
    runtime: 'systemd',
    desired_state: o.desired ?? 'stopped',
    process,
    readiness: { state: o.readiness ?? 'unknown', since: null, observed_at: null },
    port: null,
    heap_mib: null,
    installed: true,
    eula_accepted: true,
    actions: [],
  }
}

// One row per line of stage5-server-page.md §2, plus R1 and the cases
// the precedence rules call out.
describe('summarize', () => {
  const rows: [string, Server, string, string, string, string][] = [
    ['stopped', server({ process: 'stopped' }), 'Stopped', 'Ready to start.', 'stopped', 'unk'],
    ['R1 stopped unexpectedly', server({ process: 'stopped', desired: 'running' }), 'Stopped unexpectedly', 'Waystone expected this server to be running.', 'stopped-unexpectedly', 'warn'],
    ['starting', server({ process: 'starting', desired: 'running' }), 'Starting server…', 'Waiting for the service to start.', 'transition', 'warn'],
    ['readiness unknown', server({ process: 'running', readiness: 'unknown' }), 'Running — checking game readiness', "Minecraft hasn't been confirmed ready yet.", 'running', 'warn'],
    ['not ready, with since', server({ process: 'running', readiness: 'not_ready', since: ago(40) }), 'Running — game not ready yet', 'Process running for 40 s. Waiting for Minecraft to respond.', 'running', 'warn'],
    ['not ready, since unknown', server({ process: 'running', readiness: 'not_ready', since: null }), 'Running — game not ready yet', 'Waiting for Minecraft to respond.', 'running', 'warn'],
    ['ready', server({ process: 'running', readiness: 'ready' }), 'Online — ready for players', 'Minecraft is responding to its status probe.', 'online', 'ok'],
    ['readiness unsupported', server({ process: 'running', readiness: 'not_supported' }), 'Running', "Readiness checks aren't supported for this service.", 'running', 'ok'],
    ['unreachable, last known', server({ reachable: false, process: 'unknown', lastReported: 'running', observed: ago(120) }), 'Status unknown — node unreachable', 'Last known: Running, 2 min ago.', 'unknown', 'unk'],
    ['unreachable, process still fresh', server({ reachable: false, process: 'running', observed: ago(30) }), 'Status unknown — node unreachable', 'Last known: Running, 30 s ago.', 'unknown', 'unk'],
    ['unreachable, nothing known', server({ reachable: false, process: 'unknown', observed: null }), 'Status unknown — node unreachable', "Waystone can't confirm the server's current state.", 'unknown', 'unk'],
    ['unreachable, last known without a time', server({ reachable: false, process: 'unknown', lastReported: 'stopped', observed: null }), 'Status unknown — node unreachable', 'Last known: Stopped.', 'unknown', 'unk'],
    ['failed', server({ process: 'failed', desired: 'running' }), 'Server failed', 'systemd reports the service failed.', 'failed', 'bad'],
    ['stopping', server({ process: 'stopping' }), 'Stopping server…', 'Waiting for the process to exit.', 'transition', 'warn'],
    ['process unknown, node reachable', server({ process: 'unknown', lastReported: 'running' }), 'Status unknown', "Waystone hasn't had a recent report of this server's process.", 'unknown', 'unk'],
    ['other game', server({ process: 'running', readiness: 'not_ready', profile: 'valheim' }), 'Running — game not ready yet', 'Waiting for the game to respond.', 'running', 'warn'],
  ]
  it.each(rows)('%s', (_, s, title, detail, mark, tone) => {
    expect(summarize(s, now)).toEqual({ title, detail, mark, tone })
  })

  it('readiness never overrides the process (§2)', () => {
    for (const p of ['stopped', 'starting', 'stopping', 'failed', 'unknown'] as const) {
      expect(summarize(server({ process: p, readiness: 'ready' }), now).title).not.toMatch(/Online/)
    }
  })

  it('"Online" only when the game is ready (R6)', () => {
    for (const r of ['not_ready', 'unknown', 'not_supported'] as const) {
      expect(summarize(server({ process: 'running', readiness: r }), now).title).not.toMatch(/Online/)
    }
  })

  it('reachability comes first (§2)', () => {
    expect(summarize(server({ reachable: false, process: 'running', readiness: 'ready' }), now).title).toBe('Status unknown — node unreachable')
  })
})

it('formatDuration', () => {
  expect(formatDuration(0)).toBe('0 s')
  expect(formatDuration(59_999)).toBe('59 s')
  expect(formatDuration(60_000)).toBe('1 min')
  expect(formatDuration(3_599_000)).toBe('59 min')
  expect(formatDuration(3_600_000)).toBe('1 h')
  expect(formatDuration(47 * 3_600_000)).toBe('47 h')
  expect(formatDuration(72 * 3_600_000)).toBe('3 days')
  expect(formatDuration(-5000)).toBe('0 s')
})

// The B8 reasons the API sends (internal/view).
it('reasonText', () => {
  expect(reasonText('node_unreachable')).toBe('Unavailable while the node is unreachable.')
  expect(reasonText('previous_operation_uncertain')).toBe("Unavailable until an admin resolves the last operation's uncertain outcome.")
  expect(reasonText('operation_in_progress')).toBe('Unavailable while another operation on this server is in progress.')
  expect(reasonText('something_new')).toBe('Unavailable right now.')
  expect(reasonText(undefined)).toBe('Unavailable right now.')
})

// 6d: a new server reads "Not installed" until it is, and a stopped one
// that waits for the EULA says so. Unreachable and a live process still
// come first.
describe('install and EULA in the summary', () => {
  it('not installed', () => {
    for (const p of ['stopped', 'unknown', 'failed'] as const) {
      const sum = summarize({ ...server({ process: p }), installed: false, eula_accepted: false }, now)
      expect([sum.title, sum.mark]).toEqual(['Not installed', 'stopped'])
    }
    expect(summarize({ ...server({ process: 'running', readiness: 'ready' }), installed: false }, now).title).toBe('Online — ready for players')
    expect(summarize({ ...server({ process: 'stopped', reachable: false }), installed: false }, now).title).toBe('Status unknown — node unreachable')
  })
  it('waiting for the EULA', () => {
    expect(summarize({ ...server({ process: 'stopped' }), eula_accepted: false }, now).detail).toBe('It starts once the Minecraft EULA is accepted.')
    expect(summarize(server({ process: 'stopped' }), now).detail).toBe('Ready to start.')
  })
  it('the 6c reasons read as sentences', () => {
    expect(reasonText('not_installed')).toBe('Unavailable until the server is installed.')
    expect(reasonText('eula_not_accepted')).toBe('Unavailable until the Minecraft EULA is accepted.')
    expect(reasonText('node_revoked')).toBe('Unavailable: the node was revoked.')
    expect(reasonText('server_running')).toBe('Unavailable while the server is running.')
  })
})
