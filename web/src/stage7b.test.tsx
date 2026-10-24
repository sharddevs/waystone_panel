// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { readFileSync } from 'node:fs'
import { MemoryRouter, useLocation } from 'react-router'
import { describe, expect, it } from 'vitest'
import { App } from './App'
import type { Operation, Server } from './api'
import { commandProblem } from './components/CommandBox'
import { opLabel } from './format'
import { SessionProvider } from './session'
import { fakeClient, goldenSession, webError, type Call, type Reply } from './test/fakeApi'

// Stage 7b (scope D2): the console command box under the log, against
// the Go side's goldens.

const golden = <T,>(f: string): T => JSON.parse(readFileSync(`../internal/view/testdata/${f}`, 'utf8')) as T
const [survival, , testServer] = golden<{ servers: Server[] }>('servers.json').servers as [Server, Server, Server]
const commandOp = golden<{ operations: Operation[] }>('operations.json').operations[3] as Operation

function Where() {
  const l = useLocation()
  return <output data-testid="where">{l.pathname}</output>
}

function app(server: Server, onCommand: (c: Call) => Reply = () => ({ status: 202, body: { ...commandOp, state: 'pending' } })) {
  const api = fakeClient((c: Call) => {
    const url = new URL(c.path, 'http://x')
    if (c.method === 'GET' && url.pathname === '/api/v1/session') return { status: 200, body: goldenSession }
    if (c.method === 'GET' && url.pathname === `/api/v1/servers/${server.id}/logs`) return { status: 200, body: { lines: [], cursor: null, more: false } }
    if (c.method === 'GET' && url.pathname === `/api/v1/servers/${server.id}`) return { status: 200, body: server }
    if (c.method === 'GET' && url.pathname === '/api/v1/operations') return { status: 200, body: { operations: [] as Operation[] } }
    if (c.method === 'POST' && url.pathname === `/api/v1/servers/${server.id}/command`) return onCommand(c)
    return webError('not_found', 404)
  })
  render(
    <MemoryRouter initialEntries={[`/servers/${server.id}`]}>
      <SessionProvider client={api.client}>
        <App />
        <Where />
      </SessionProvider>
    </MemoryRouter>,
  )
  return api
}

const input = () => screen.findByRole('textbox', { name: 'Console command' })
const send = () => screen.getByRole('button', { name: 'Send' })
const posts = (api: ReturnType<typeof app>) => api.calls.filter((c) => c.method === 'POST')

describe('command box (D2)', () => {
  it('sends the line with a fresh key and the CSRF token, then clears', async () => {
    const api = app(survival)
    await userEvent.type(await input(), 'say Back in five minutes{Enter}')
    await waitFor(() => expect(posts(api)).toHaveLength(1))
    const [c] = posts(api) as [Call]
    expect(c.body).toEqual({ command: 'say Back in five minutes' })
    expect(c.headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/)
    expect(c.headers['X-CSRF-Token']).toBe(goldenSession.csrf_token)
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Console command' })).toHaveValue(''))
    // The accepted command is the active line, with its line.
    expect(await screen.findByRole('link', { name: /Command: say Back in five minutes · pending/ })).toBeInTheDocument()

    // A second send is another key.
    await userEvent.type(screen.getByRole('textbox', { name: 'Console command' }), 'list')
    await userEvent.click(send())
    await waitFor(() => expect(posts(api)).toHaveLength(2))
    const [first, second] = posts(api) as [Call, Call]
    expect(second.headers['Idempotency-Key']).not.toBe(first.headers['Idempotency-Key'])
  })

  it('says what is wrong with a line before sending it', async () => {
    const api = app(survival)
    const box = await input()
    await userEvent.type(box, '/stop')
    expect(screen.getByText(/Leave out the leading \//)).toBeInTheDocument()
    expect(box).toHaveAttribute('aria-invalid', 'true')
    expect(send()).toBeDisabled()
    await userEvent.type(box, '{Enter}')
    expect(posts(api)).toHaveLength(0)
    // Empty: disabled, with nothing to complain about yet.
    await userEvent.clear(box)
    expect(send()).toBeDisabled()
    expect(box).toHaveAttribute('aria-invalid', 'false')
  })

  it('the line rules match console.ValidLine', () => {
    expect(commandProblem('say hi é')).toBeUndefined()
    expect(commandProblem('a'.repeat(256))).toBeUndefined()
    expect(commandProblem('é'.repeat(128))).toBeUndefined()
    expect(commandProblem('é'.repeat(129))).toMatch(/at most 256 bytes; this one is 258/)
    expect(commandProblem('a'.repeat(257))).toBeDefined()
    expect(commandProblem('')).toBeDefined()
    expect(commandProblem('/stop')).toBeDefined()
    for (const bad of ['say a\nstop', 'say\ta', 'say\u0000', 'say\u001b[2J', 'say\u007f', 'say\u0085', 'say \u202eboo', 'say\u2066', 'say\u200e', 'say\u061c']) {
      expect(commandProblem(bad), JSON.stringify(bad)).toBeDefined()
    }
    // A slash later in the line is fine.
    expect(commandProblem('say a/b')).toBeUndefined()
  })

  it('the API refusal is shown, and the line is kept', async () => {
    app(survival, () => webError('server_not_running', 409))
    await userEvent.type(await input(), 'say hi{Enter}')
    expect(await screen.findByText('Commands can only be sent while the server is running.')).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'Console command' })).toHaveValue('say hi')
  })

  it('a session that ends on send goes to login', async () => {
    app(survival, () => webError('unauthenticated', 401))
    await userEvent.type(await input(), 'say hi{Enter}')
    await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent('/login'))
  })

  it('disabled with its reason when the state refuses it', async () => {
    app(testServer)
    const box = await input()
    expect(box).toBeDisabled()
    expect(send()).toBeDisabled()
    const log = screen.getByRole('region', { name: 'Log' })
    expect(within(log).getByText('Unavailable while another operation on this server is in progress.')).toBeInTheDocument()
  })

  it('no command action: no box (not an admin)', async () => {
    app({ ...survival, actions: survival.actions.filter((a) => a.action !== 'command') })
    await screen.findByRole('heading', { level: 1, name: 'Survival World' })
    await screen.findByRole('region', { name: 'Server log' })
    expect(screen.queryByRole('textbox', { name: 'Console command' })).toBeNull()
  })

  it('a stopped server says why', async () => {
    const actions = survival.actions.map((a) => (a.action === 'command' ? { action: a.action, allowed: false, reason: 'server_not_running' } : a))
    app({ ...survival, actions })
    expect(await input()).toBeDisabled()
    expect(screen.getByText("Unavailable while the server isn't running.")).toBeInTheDocument()
  })
})

describe('command operations (D2)', () => {
  it('are labelled with their line, cleaned', () => {
    expect(opLabel(commandOp)).toBe('Command: say Back in five minutes')
    expect(opLabel({ ...commandOp, command: 'say \u202eboo' })).toBe('Command: say \uFFFDboo')
    const noLine: Operation = { ...commandOp }
    delete noLine.command
    expect(opLabel(noLine)).toBe('Console command')
  })

  it('the operation page shows the line', async () => {
    const api = fakeClient((c: Call) => {
      const url = new URL(c.path, 'http://x')
      if (url.pathname === '/api/v1/session') return { status: 200, body: goldenSession }
      if (url.pathname === `/api/v1/operations/${commandOp.id}`) return { status: 200, body: commandOp }
      if (url.pathname === `/api/v1/servers/${commandOp.server_id}`) return { status: 200, body: survival }
      return webError('not_found', 404)
    })
    render(
      <MemoryRouter initialEntries={[`/operations/${commandOp.id}`]}>
        <SessionProvider client={api.client}>
          <App />
        </SessionProvider>
      </MemoryRouter>,
    )
    expect(await screen.findByRole('heading', { level: 1, name: /Console command/ })).toBeInTheDocument()
    expect(screen.getByText('say Back in five minutes')).toHaveClass('mono')
  })
})
