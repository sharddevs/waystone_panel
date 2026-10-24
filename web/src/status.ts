// SPDX-License-Identifier: AGPL-3.0-only

// The server status summary (notes/stage5-server-page.md §2, R1, R6).
// One pure function turns the three dimensions into what a person reads.
// The order is fixed: reachability first, then the process, then
// readiness. "Online" only ever means the game is ready.

import type { ProcessState, Server } from './api'

// Mark is the symbol drawn next to the words (design tokens, "Status
// marks"). Tone is the colour token.
export type Mark = 'online' | 'running' | 'transition' | 'stopped' | 'stopped-unexpectedly' | 'unknown' | 'failed'
export type Tone = 'ok' | 'warn' | 'bad' | 'unk'

export interface Summary {
  mark: Mark
  tone: Tone
  title: string
  detail: string
}

// gameName is the game's name for a profile, for sentences like
// "Waiting for Minecraft to respond".
export function gameName(profileId: string): string {
  if (profileId === 'minecraft-java' || profileId.startsWith('minecraft')) return 'Minecraft'
  return 'the game'
}

const processWords: Record<ProcessState, string> = {
  stopped: 'Stopped',
  starting: 'Starting',
  running: 'Running',
  stopping: 'Stopping',
  failed: 'Failed',
  unknown: 'Unknown',
}

// formatDuration says how long ms is, roughly: "8 s", "4 min", "2 h",
// "3 days". It is only ever given a real duration.
export function formatDuration(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 60) return `${s} s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h`
  return `${Math.floor(h / 24)} days`
}

// since returns how long ago t was, or undefined when t is unknown: an
// elapsed time is never made up (§2, "Timestamps matter").
function ago(t: string | null, now: Date): string | undefined {
  if (t === null) return undefined
  const ms = now.getTime() - Date.parse(t)
  return Number.isNaN(ms) ? undefined : formatDuration(ms)
}

export function summarize(s: Server, now: Date): Summary {
  const game = gameName(s.profile.id)
  const proc = s.process

  if (s.node.reachability.state === 'unreachable') {
    const last = proc.state !== 'unknown' ? proc.state : proc.last_reported
    let detail = "Waystone can't confirm the server's current state."
    if (last !== undefined && last !== 'unknown') {
      const when = ago(proc.observed_at, now)
      detail = when === undefined ? `Last known: ${processWords[last]}.` : `Last known: ${processWords[last]}, ${when} ago.`
    }
    return { mark: 'unknown', tone: 'unk', title: 'Status unknown — node unreachable', detail }
  }

  // A new server: nothing to run until it is installed (6c), and it
  // won't start until the EULA is accepted. A process that is up is
  // still reported as it is below.
  if (!s.installed && (proc.state === 'stopped' || proc.state === 'unknown' || proc.state === 'failed')) {
    return { mark: 'stopped', tone: 'unk', title: 'Not installed', detail: 'Waystone installs the server before it can start.' }
  }

  switch (proc.state) {
    case 'unknown':
      return {
        mark: 'unknown',
        tone: 'unk',
        title: 'Status unknown',
        detail: "Waystone hasn't had a recent report of this server's process.",
      }
    case 'failed':
      return { mark: 'failed', tone: 'bad', title: 'Server failed', detail: 'systemd reports the service failed.' }
    case 'stopped':
      if (s.desired_state === 'running') {
        return {
          mark: 'stopped-unexpectedly',
          tone: 'warn',
          title: 'Stopped unexpectedly',
          detail: 'Waystone expected this server to be running.',
        }
      }
      return {
        mark: 'stopped',
        tone: 'unk',
        title: 'Stopped',
        detail: s.eula_accepted ? 'Ready to start.' : 'It starts once the Minecraft EULA is accepted.',
      }
    case 'starting':
      return { mark: 'transition', tone: 'warn', title: 'Starting server…', detail: 'Waiting for the service to start.' }
    case 'stopping':
      return { mark: 'transition', tone: 'warn', title: 'Stopping server…', detail: 'Waiting for the process to exit.' }
    case 'running':
      break
  }

  // Running: readiness decides, but never overrides the process state.
  switch (s.readiness.state) {
    case 'ready':
      return { mark: 'online', tone: 'ok', title: 'Online — ready for players', detail: `${cap(game)} is responding to its status probe.` }
    case 'not_ready': {
      const running = ago(proc.since, now)
      const wait = `Waiting for ${game} to respond.`
      return {
        mark: 'running',
        tone: 'warn',
        title: 'Running — game not ready yet',
        detail: running === undefined ? wait : `Process running for ${running}. ${wait}`,
      }
    }
    case 'not_supported':
      return { mark: 'running', tone: 'ok', title: 'Running', detail: "Readiness checks aren't supported for this service." }
    case 'unknown':
      return {
        mark: 'running',
        tone: 'warn',
        title: 'Running — checking game readiness',
        detail: `${cap(game)} hasn't been confirmed ready yet.`,
      }
  }
}

function cap(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1)
}

// reasonText is the sentence under a disabled action (B8). An unknown
// reason still reads as a reason.
export function reasonText(reason: string | undefined): string {
  switch (reason) {
    case 'node_unreachable':
      return 'Unavailable while the node is unreachable.'
    case 'previous_operation_uncertain':
      return "Unavailable until an admin resolves the last operation's uncertain outcome."
    case 'operation_in_progress':
      return 'Unavailable while another operation on this server is in progress.'
    case 'node_revoked':
      return 'Unavailable: the node was revoked.'
    case 'not_installed':
      return 'Unavailable until the server is installed.'
    case 'eula_not_accepted':
      return 'Unavailable until the Minecraft EULA is accepted.'
    case 'server_running':
      return 'Unavailable while the server is running.'
    case 'already_accepted':
      return 'The EULA is already accepted.'
    case 'server_not_running':
      return "Unavailable while the server isn't running."
    default:
      return 'Unavailable right now.'
  }
}

// dimensionsLine is the three dimensions in plain words, one line under
// the summary (wireframes §3): "Node reachable · Process running for
// 12 min · Game ready". Durations come only from real timestamps.
export function dimensionsLine(s: Server, now: Date): string {
  const parts: string[] = []
  parts.push(s.node.reachability.state === 'reachable' ? 'Node reachable' : 'Node unreachable')
  const p = s.process
  if (p.state === 'unknown') parts.push('Process state unknown')
  else {
    const forHow = p.state === 'running' ? ago(p.since, now) : undefined
    parts.push(forHow === undefined ? `Process ${p.state}` : `Process running for ${forHow}`)
  }
  switch (s.readiness.state) {
    case 'ready':
      parts.push('Game ready')
      break
    case 'not_ready':
      parts.push('Game not ready')
      break
    case 'unknown':
      parts.push('Game readiness unknown')
      break
    case 'not_supported':
      parts.push('No readiness check')
      break
  }
  return parts.join(' · ')
}
