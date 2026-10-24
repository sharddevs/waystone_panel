// SPDX-License-Identifier: AGPL-3.0-only

import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { readFileSync } from 'node:fs'
import { MemoryRouter, useLocation } from 'react-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'
import type { Operation, Server } from './api'
import { noteProblem } from './pages/OperationPage'
import { SessionProvider } from './session'
import { fakeClient, goldenSession, webError, type Call, type Reply } from './test/fakeApi'

const golden = <T,>(f: string): T => JSON.parse(readFileSync(`../internal/view/testdata/${f}`, 'utf8')) as T
const servers = () => golden<{ servers: Server[] }>('servers.json').servers
const operations = () => golden<{ operations: Operation[] }>('operations.json').operations
const [survival, modded] = servers() as [Server, Server, Server]
const [restarting, failedStop, uncertainStart] = operations() as [Operation, Operation, Operation]

function Where() {
  const l = useLocation()
  return <output data-testid="where">{l.pathname + l.search}</output>
}

// The goldens are "now" = 2026-10-16 12:00 UTC. Only Date is faked; the
// timers stay real unless a test says otherwise.
const goldenNow = new Date('2026-10-16T12:00:00Z')
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(goldenNow)
})
afterEach(() => {
  vi.useRealTimers()
})

type Handler = (c: Call) => Reply | Promise<Reply> | undefined

// signedIn is a controller with trevor signed in. Routes not answered by
// over: the session, the golden servers and operations by id.
function app(at: string, over: Handler = () => undefined) {
  const api = fakeClient((c) => {
    const o = over(c)
    if (o) return o
    const path = c.path.split('?')[0] ?? ''
    if (c.method === 'GET' && path === '/api/v1/session') return { status: 200, body: { ...goldenSession, preferences: { theme: 'stone', density: 'comfortable', mode: 'dark' } } }
    if (c.method === 'GET' && path === '/api/v1/servers') return { status: 200, body: { servers: servers() } }
    if (c.method === 'GET' && path.startsWith('/api/v1/servers/')) {
      const s = servers().find((x) => `/api/v1/servers/${x.id}` === path)
      return s ? { status: 200, body: s } : webError('not_found', 404)
    }
    if (c.method === 'GET' && path === '/api/v1/operations') return { status: 200, body: { operations: operations() } }
    if (c.method === 'GET' && path.startsWith('/api/v1/operations/')) {
      const o = operations().find((x) => `/api/v1/operations/${x.id}` === path)
      return o ? { status: 200, body: o } : webError('not_found', 404)
    }
    return webError('not_found', 404)
  })
  render(
    <MemoryRouter initialEntries={[at]}>
      <SessionProvider client={api.client}>
        <App />
        <Where />
      </SessionProvider>
    </MemoryRouter>,
  )
  return api
}

const serverPath = (s: Server) => `/servers/${s.id}`
const opPath = (o: Operation) => `/operations/${o.id}`

describe('server list (wireframes §2)', () => {
  it('lists every server by name, each row a link with its status, game and node', async () => {
    app('/')
    await screen.findByText('3 servers')
    const list = screen.getByRole('list')
    const rows = within(list).getAllByRole('link')
    expect(rows.map((r) => r.querySelector('.cell-name')?.textContent)).toEqual(['Modded', 'Survival World', 'Test'])
    expect(screen.getByText('3 servers')).toBeInTheDocument()
    const online = rows[1] as HTMLElement
    expect(online).toHaveAttribute('href', serverPath(survival))
    expect(online).toHaveTextContent('Online — ready for players')
    expect(online).toHaveTextContent('Minecraft Java 26.3')
    expect(online).toHaveTextContent('homelab-1')
    // Unknown says why, on its own line; no version when it is unknown (V3).
    const unknown = rows[0] as HTMLElement
    expect(unknown).toHaveTextContent('Status unknown — node unreachable')
    expect(unknown.querySelector('.why')).toHaveTextContent('Last known: Running, 4 min ago.')
    expect(unknown.querySelector('.cell-game')).toHaveTextContent(/^Minecraft Java$/)
    // No action buttons in the list.
    expect(within(list).queryByRole('button')).toBeNull()
    expect(document.title).toBe('Servers · Waystone')
  })

  it('shows placeholder rows while loading, never a covering spinner', async () => {
    let release: () => void = () => {}
    app('/', (c) => (c.path === '/api/v1/servers' ? new Promise<Reply>((r) => (release = () => r({ status: 200, body: { servers: [] } }))) : undefined))
    const busy = await screen.findByLabelText('Loading servers')
    expect(busy).toHaveAttribute('aria-busy', 'true')
    expect(busy.querySelectorAll('li')).toHaveLength(3)
    await act(async () => release())
    expect(await screen.findByText('No servers yet. Create one, and Waystone installs it on the node you pick.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Create server' })).toHaveAttribute('href', '/servers/new')
  })

  it("can't load: says so with the request ID, and tries again", async () => {
    let fail = true
    const { calls } = app('/', (c) =>
      c.path === '/api/v1/servers' && fail ? { status: 503, body: { error: { code: 'controller_busy', message: 'x', request_id: 'req-123' } } } : undefined,
    )
    expect(await screen.findByText(/Couldn't load servers\./)).toBeInTheDocument()
    expect(screen.getByText('req-123')).toBeInTheDocument()
    fail = false
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByText('3 servers')).toBeInTheDocument()
    expect(calls.filter((c) => c.path === '/api/v1/servers')).toHaveLength(2)
  })

  it('refreshes every 5 s, and keeps the last list when a refresh fails', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    vi.setSystemTime(goldenNow)
    try {
      let fail = false
      const { calls } = app('/', (c) => (c.path === '/api/v1/servers' && fail ? webError('controller_busy', 503) : undefined))
      expect(await screen.findByText('3 servers')).toBeInTheDocument()
      fail = true
      await act(async () => vi.advanceTimersByTimeAsync(5000))
      expect(calls.filter((c) => c.path === '/api/v1/servers')).toHaveLength(2)
      expect(screen.getByText('3 servers')).toBeInTheDocument()
      expect(screen.getByText(/Couldn't refresh/)).toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('server page (wireframes §3)', () => {
  it('shows identity, the status summary, the three dimensions and the raw details', async () => {
    app(serverPath(survival))
    expect(await screen.findByRole('heading', { level: 1, name: 'Survival World' })).toBeInTheDocument()
    expect(screen.getByText('Minecraft Java 26.3')).toBeInTheDocument()
    expect(screen.getByText('on homelab-1 · systemd')).toBeInTheDocument()
    const card = screen.getByRole('region', { name: 'Status' })
    expect(card).toHaveTextContent('Online — ready for players')
    expect(card).toHaveTextContent('Node reachable · Process running for 12 min · Game ready')
    expect(card).toHaveTextContent('Checked 8 s ago')
    const details = card.querySelector('details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    await userEvent.click(within(card).getByText('Details'))
    expect(details.open).toBe(true)
    expect(within(details).getByText('Process').nextElementSibling).toHaveTextContent(/^running, since .*, observed /)
    expect(document.title).toBe('Survival World · Waystone')
    expect(within(screen.getByRole('navigation', { name: 'Main' })).getByRole('link', { name: 'Servers' })).toHaveAttribute('aria-current', 'page')
  })

  it('buttons come from actions: allowed ones work, blocked ones say why, missing ones are not there', async () => {
    app(serverPath(modded))
    await screen.findByRole('heading', { level: 1, name: 'Modded' })
    expect(screen.getByRole('button', { name: 'Start' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Stop' })).toBeDisabled()
    // Restart isn't in this server's actions: not permitted, so not shown (R4).
    expect(screen.queryByRole('button', { name: 'Restart' })).toBeNull()
    expect(screen.getByText('Unavailable while the node is unreachable.')).toBeInTheDocument()
    expect(screen.getAllByText('Unavailable while the node is unreachable.')).toHaveLength(1)
  })

  it('install, the EULA and commands are listed by the API but get no button among the actions (6d, 7b)', async () => {
    app(serverPath(survival))
    await screen.findByRole('heading', { level: 1, name: 'Survival World' })
    expect(survival.actions.map((a) => a.action)).toEqual(['start', 'stop', 'restart', 'install', 'accept_eula', 'command'])
    expect(screen.getAllByRole('button', { name: /^(Start|Stop|Restart)$/ })).toHaveLength(3)
    expect(screen.queryByRole('button', { name: /install|eula/i })).toBeNull()
  })

  it('no permitted actions at all: no buttons', async () => {
    app(serverPath(survival), (c) => (c.path === `/api/v1/servers/${survival.id}` ? { status: 200, body: { ...survival, actions: [] } } : undefined))
    await screen.findByRole('heading', { level: 1, name: 'Survival World' })
    expect(screen.queryByRole('button', { name: /Start|Stop|Restart/ })).toBeNull()
  })

  it('Start: sends one request with CSRF and a fresh key, shows it is working, then the active operation', async () => {
    let release: () => void = () => {}
    const accepted: Operation = { ...restarting, id: '0199e1a0-0000-7000-8000-00000000d0ff', action: 'server.start', state: 'pending', started_at: null }
    delete accepted.phase
    const { calls } = app(serverPath(survival), (c) => {
      if (c.method === 'POST') return new Promise<Reply>((r) => (release = () => r({ status: 202, body: accepted })))
      if (c.path.startsWith('/api/v1/operations?')) return { status: 200, body: { operations: [failedStop] } }
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: 'Start' }))
    expect(screen.getByRole('button', { name: 'Starting…' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Stop' })).toBeDisabled()
    const post = calls.filter((c) => c.method === 'POST')
    expect(post).toHaveLength(1)
    expect(post[0]?.path).toBe(`/api/v1/servers/${survival.id}/start`)
    expect(post[0]?.headers['X-CSRF-Token']).toBe(goldenSession.csrf_token)
    expect(post[0]?.headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/)
    expect(post[0]?.body).toEqual({})
    await act(async () => release())
    const line = await screen.findByRole('link', { name: /Start server · pending/ })
    expect(line).toHaveAttribute('href', `/operations/${accepted.id}`)
    expect(screen.getByRole('button', { name: 'Start' })).toBeEnabled()

    // A second click is a new request with a new key.
    await userEvent.click(screen.getByRole('button', { name: 'Start' }))
    await act(async () => release())
    const keys = calls.filter((c) => c.method === 'POST').map((c) => c.headers['Idempotency-Key'])
    expect(keys).toHaveLength(2)
    expect(keys[0]).not.toBe(keys[1])
  })

  it('the active line shows the running operation and its phase (R2)', async () => {
    app(serverPath(survival))
    const line = await screen.findByRole('link', { name: 'Restart server · running · stopping (9 min)' })
    expect(line).toHaveAttribute('href', opPath(restarting))
  })

  it('a refused action says why, and links to the operation in the way', async () => {
    app(serverPath(survival), (c) =>
      c.method === 'POST'
        ? { status: 409, body: { error: { code: 'operation_in_progress', message: 'x', request_id: 'req-9', operation_id: restarting.id } } }
        : undefined,
    )
    await userEvent.click(await screen.findByRole('button', { name: 'Stop' }))
    expect(await screen.findByText('Another operation on this server is still in progress.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Open that operation' })).toHaveAttribute('href', opPath(restarting))
    expect(screen.getByText('req-9')).toBeInTheDocument()
  })

  it('an uncertain last operation: says actions wait for an admin, and links to it (F13)', async () => {
    const blocked = { ...survival, actions: survival.actions.map((a) => ({ ...a, allowed: false, reason: 'previous_operation_uncertain' })) }
    const open = { ...uncertainStart, resolution: null }
    app(serverPath(survival), (c) => {
      if (c.path === `/api/v1/servers/${survival.id}`) return { status: 200, body: blocked }
      if (c.path.startsWith('/api/v1/operations?')) return { status: 200, body: { operations: [open] } }
      return undefined
    })
    expect(await screen.findByText("The last operation's outcome is uncertain.")).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Review operation' })).toHaveAttribute('href', opPath(uncertainStart))
    expect(screen.getByRole('button', { name: 'Start' })).toBeDisabled()
  })

  it('an uncertain operation is called out even when an unreachable node is the reason named', async () => {
    const open = { ...uncertainStart, server_id: modded.id, resolution: null }
    app(serverPath(modded), (c) => (c.path.startsWith('/api/v1/operations?') ? { status: 200, body: { operations: [open] } } : undefined))
    expect(await screen.findByText("The last operation's outcome is uncertain.")).toBeInTheDocument()
    expect(screen.getByText('Unavailable while the node is unreachable.')).toBeInTheDocument()
  })

  it('a resolved uncertain operation is no longer called out', async () => {
    app(serverPath(modded), (c) => (c.path.startsWith('/api/v1/operations?') ? { status: 200, body: { operations: [uncertainStart] } } : undefined))
    await screen.findByText('Outcome uncertain')
    expect(screen.queryByText("The last operation's outcome is uncertain.")).toBeNull()
  })

  it('history: action, state, time, who, and the error or duration; Show more asks for more', async () => {
    const ten = Array.from({ length: 10 }, (_, i) => ({ ...failedStop, id: `0199e1a0-0000-7000-8000-0000000000${(10 + i).toString()}` }))
    const { calls } = app(serverPath(survival), (c) =>
      c.path.startsWith('/api/v1/operations?') ? { status: 200, body: { operations: c.path.includes('limit=10') ? ten : operations() } } : undefined,
    )
    const history = await screen.findByRole('region', { name: 'Recent operations' })
    const rows = await within(history).findAllByRole('link')
    expect(rows).toHaveLength(10)
    expect(rows[0]).toHaveTextContent('Stop server')
    expect(rows[0]).toHaveTextContent('Failed')
    expect(rows[0]).toHaveTextContent('trevor')
    expect(rows[0]).toHaveTextContent('systemd was busy with this server.')
    const first = calls.find((c) => c.path.startsWith('/api/v1/operations?'))
    expect(first?.path).toBe(`/api/v1/operations?server_id=${survival.id}&limit=10`)
    await userEvent.click(within(history).getByRole('button', { name: 'Show more' }))
    await waitFor(() => expect(calls.some((c) => c.path === `/api/v1/operations?server_id=${survival.id}&limit=50`)).toBe(true))
    // Fewer than asked for: that's everything, so no more "Show more".
    await waitFor(() => expect(within(history).getAllByRole('link')).toHaveLength(4))
    expect(within(history).queryByRole('button', { name: 'Show more' })).toBeNull()
    // Uncertain is its own word, never failed or succeeded.
    expect(within(history).getAllByRole('link')[2]).toHaveTextContent('Outcome uncertain')
    // A console command shows its line (7b).
    expect(within(history).getAllByRole('link')[3]).toHaveTextContent('Command: say Back in five minutes')
  })

  it('a missing server says so', async () => {
    app('/servers/0199e1a0-0000-7000-8000-00000000ffff')
    expect(await screen.findByText(/Couldn't find this server\./)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '← Servers' })).toHaveAttribute('href', '/')
  })

  it('a session that ends while the page refreshes goes to login (W5)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    vi.setSystemTime(goldenNow)
    try {
      let gone = false
      app(serverPath(survival), () => (gone ? webError('unauthenticated', 401) : undefined))
      await screen.findByRole('heading', { level: 1, name: 'Survival World' })
      gone = true
      await act(async () => vi.advanceTimersByTimeAsync(5000))
      await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent(`/login?next=${encodeURIComponent(serverPath(survival))}`))
    } finally {
      vi.useRealTimers()
    }
  })

  it('an action refused with 401 goes to login, even while reads still work, and reloads nothing', async () => {
    const { calls } = app(serverPath(survival), (c) => (c.method === 'POST' ? webError('unauthenticated', 401) : undefined))
    await userEvent.click(await screen.findByRole('button', { name: 'Stop' }))
    await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent(`/login?next=${encodeURIComponent(serverPath(survival))}`))
    const post = calls.findIndex((c) => c.method === 'POST')
    expect(calls.slice(post + 1).filter((c) => c.path.startsWith('/api/v1/servers/') || c.path.startsWith('/api/v1/operations'))).toHaveLength(0)
  })

  it('a session that ends goes to login, and comes back here afterwards', async () => {
    let gone = false
    app(serverPath(survival), () => {
      if (!gone) return undefined
      return webError('unauthenticated', 401)
    })
    await screen.findByRole('heading', { level: 1, name: 'Survival World' })
    gone = true
    await userEvent.click(screen.getByRole('button', { name: 'Stop' }))
    await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent(`/login?next=${encodeURIComponent(serverPath(survival))}`))
    expect(await screen.findByText('Your session ended. Sign in again.')).toBeInTheDocument()
  })
})

describe('operation details (wireframes §4)', () => {
  it('a failed operation: what, when, who, what happened, and the ID at the bottom', async () => {
    app(opPath(failedStop))
    const h = await screen.findByRole('heading', { level: 1 })
    expect(h).toHaveTextContent('Stop server')
    expect(h).toHaveTextContent('Failed')
    expect(await screen.findByRole('link', { name: '← Survival World' })).toHaveAttribute('href', serverPath(survival))
    expect(screen.getByText(/by trevor/)).toBeInTheDocument()
    expect(screen.getByText(/\(7\.1 s\)/)).toBeInTheDocument()
    const what = screen.getByRole('region', { name: 'What happened' })
    expect(what).toHaveTextContent('systemd was busy with this server. (server_busy)')
    expect(screen.getByText(failedStop.id)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Copy' })).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Resolve this operation' })).toBeNull()
  })

  it('a running restart shows its live phase, not "Finished"', async () => {
    app(opPath(restarting))
    expect(await screen.findByText('Now stopping. Elapsed 9 min.')).toBeInTheDocument()
    expect(screen.queryByText('Finished')).toBeNull()
  })

  it('a resolved uncertain operation stays uncertain, with the resolution under it (C2, C7)', async () => {
    app(opPath(uncertainStart))
    const h = await screen.findByRole('heading', { level: 1 })
    expect(h).toHaveTextContent('Outcome uncertain')
    expect(screen.getByText("Waystone lost contact while this was running, so it doesn't know whether the server started.")).toBeInTheDocument()
    const r = uncertainStart.resolution
    if (r === null) throw new Error('golden changed')
    expect(screen.getByRole('region', { name: 'Resolved' })).toHaveTextContent(`Marked as ${r.outcome} by ${r.by.name}`)
    expect(screen.queryByRole('region', { name: 'Resolve this operation' })).toBeNull()
    expect(screen.getByText('Not known')).toBeInTheDocument()
  })

  it('resolve: the note is required, then it sends outcome and note with CSRF, and shows the result', async () => {
    let resolved: Operation['resolution'] = null
    const { calls } = app(opPath(uncertainStart), (c) => {
      if (c.method === 'POST') {
        const b = c.body as { outcome: 'succeeded' | 'failed'; note: string }
        resolved = { at: '2026-10-16T12:00:00Z', outcome: b.outcome, by: { type: 'user', id: 'u1', name: 'Trevor' }, note: b.note }
        return { status: 200, body: { ...uncertainStart, resolution: resolved } }
      }
      if (c.path === `/api/v1/operations/${uncertainStart.id}`) return { status: 200, body: { ...uncertainStart, resolution: resolved } }
      return undefined
    })
    const form = await screen.findByRole('region', { name: 'Resolve this operation' })
    await userEvent.click(within(form).getByRole('button', { name: 'Mark as failed' }))
    expect(within(form).getByText('Write a note: what you checked, and what you found.')).toBeInTheDocument()
    expect(within(form).getByLabelText('Note (required)')).toHaveAttribute('aria-invalid', 'true')
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)

    await userEvent.type(within(form).getByLabelText('Note (required)'), 'Checked the console; it never came up.')
    await userEvent.click(within(form).getByRole('button', { name: 'Mark as failed' }))
    const post = calls.filter((c) => c.method === 'POST')
    expect(post).toHaveLength(1)
    expect(post[0]?.path).toBe(`/api/v1/operations/${uncertainStart.id}/resolve`)
    expect(post[0]?.body).toEqual({ outcome: 'failed', note: 'Checked the console; it never came up.' })
    expect(post[0]?.headers['X-CSRF-Token']).toBe(goldenSession.csrf_token)
    expect(post[0]?.headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/)
    expect(await screen.findByRole('region', { name: 'Resolved' })).toHaveTextContent('Marked as failed by Trevor')
    expect(screen.getByRole('region', { name: 'Resolved' })).toHaveTextContent('“Checked the console; it never came up.”')
    expect(screen.queryByRole('region', { name: 'Resolve this operation' })).toBeNull()
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Outcome uncertain')
  })

  it('resolve refused: says why (a non-admin, later)', async () => {
    app(opPath(uncertainStart), (c) => {
      if (c.method === 'POST') return webError('forbidden', 403)
      if (c.path === `/api/v1/operations/${uncertainStart.id}`) return { status: 200, body: { ...uncertainStart, resolution: null } }
      return undefined
    })
    const form = await screen.findByRole('region', { name: 'Resolve this operation' })
    await userEvent.type(within(form).getByLabelText('Note (required)'), 'ok')
    await userEvent.click(within(form).getByRole('button', { name: 'Mark as succeeded' }))
    expect(await within(form).findByText("You don't have permission to do this.")).toBeInTheDocument()
  })

  it('a resolve refused with 401 goes to login', async () => {
    app(opPath(uncertainStart), (c) => {
      if (c.method === 'POST') return webError('unauthenticated', 401)
      if (c.path === `/api/v1/operations/${uncertainStart.id}`) return { status: 200, body: { ...uncertainStart, resolution: null } }
      return undefined
    })
    const form = await screen.findByRole('region', { name: 'Resolve this operation' })
    await userEvent.type(within(form).getByLabelText('Note (required)'), 'ok')
    await userEvent.click(within(form).getByRole('button', { name: 'Mark as succeeded' }))
    await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent(`/login?next=${encodeURIComponent(opPath(uncertainStart))}`))
  })

  it('an operation whose server is gone still reads, with the server ID', async () => {
    app(opPath(failedStop), (c) => (c.path === `/api/v1/servers/${survival.id}` ? webError('not_found', 404) : undefined))
    await screen.findByRole('heading', { level: 1 })
    expect(screen.getByRole('link', { name: '← Servers' })).toBeInTheDocument()
    expect(screen.getByText(survival.id)).toBeInTheDocument()
  })

  it('an unknown operation says so', async () => {
    app('/operations/0199e1a0-0000-7000-8000-00000000ffff')
    expect(await screen.findByText(/Couldn't find this operation\./)).toBeInTheDocument()
  })
})

describe('noteProblem (store.validNote, said sooner)', () => {
  it.each([
    ['', 'Write a note'],
    ['   \n ', 'Write a note'],
    ['a'.repeat(501), 'too long'],
    ['é'.repeat(251), 'too long'],
    ['tab\there', 'plain text'],
    ['bell\u0007', 'plain text'],
    ['c1\u0085', 'plain text'],
    ['rlo‮', 'plain text'],
    ['lri⁦', 'plain text'],
    ['alm؜', 'plain text'],
    ['lrm‎', 'plain text'],
  ])('%j is refused', (note, why) => {
    expect(noteProblem(note)).toContain(why)
  })

  it.each([['ok'], ['two\nlines'], ['a'.repeat(500)], ['é'.repeat(250)], ['日本語 ✓']])('%j is fine', (note) => {
    expect(noteProblem(note)).toBeUndefined()
  })
})

it('the login page tells a first-time visitor how the first account is made (W4)', async () => {
  app('/login', (c) => (c.path === '/api/v1/session' ? webError('unauthenticated', 401) : undefined))
  expect(await screen.findByText(/No account yet\?/)).toHaveTextContent('waystone-controller user bootstrap')
})
