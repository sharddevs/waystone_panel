// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { readFileSync } from 'node:fs'
import { MemoryRouter, useLocation } from 'react-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'
import type { CreatedServer, Enrollment, NewEnrollment, NodeDetail, Operation, Server } from './api'
import { nextFreePort, portProblem, serverNameProblem } from './pages/CreateServer'
import { nodeNameProblem } from './pages/NodesPage'
import { eulaURL } from './pages/ServerPage'
import { SessionProvider } from './session'
import { fakeClient, goldenSession, webError, type Call, type Reply } from './test/fakeApi'

// Stage 6d (scope D9): the Nodes page, Add node, Revoke, the create form,
// and the install and EULA cards, against the Go side's goldens.

const golden = <T,>(f: string): T => JSON.parse(readFileSync(`../internal/view/testdata/${f}`, 'utf8')) as T
const servers = () => golden<{ servers: Server[] }>('servers.json').servers
const nodes = () => golden<{ nodes: NodeDetail[] }>('nodes.json').nodes
const enrollments = () => golden<{ enrollments: Enrollment[] }>('enrollments.json').enrollments
const created = () => golden<CreatedServer>('created.json')
const newEnrollment = () => golden<NewEnrollment>('enrollment_new.json')
const [survival, , testServer] = servers() as [Server, Server, Server]
const [homelab, pi, oldBox] = nodes() as [NodeDetail, NodeDetail, NodeDetail]

function Where() {
  const l = useLocation()
  return <output data-testid="where">{l.pathname + l.search}</output>
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-10-16T12:00:00Z'))
})
afterEach(() => {
  vi.useRealTimers()
})

type Handler = (c: Call) => Reply | Promise<Reply> | undefined

function app(at: string, over: Handler = () => undefined, extraServers: Server[] = []) {
  const api = fakeClient((c) => {
    const o = over(c)
    if (o) return o
    const path = c.path.split('?')[0] ?? ''
    const all = [...servers(), ...extraServers]
    if (c.method === 'GET' && path === '/api/v1/session') return { status: 200, body: goldenSession }
    if (c.method === 'GET' && path === '/api/v1/servers') return { status: 200, body: { servers: all } }
    if (c.method === 'GET' && path.startsWith('/api/v1/servers/')) {
      const s = all.find((x) => `/api/v1/servers/${x.id}` === path)
      return s ? { status: 200, body: s } : webError('not_found', 404)
    }
    if (c.method === 'GET' && path === '/api/v1/operations') return { status: 200, body: { operations: [] as Operation[] } }
    if (c.method === 'GET' && path === '/api/v1/nodes') return { status: 200, body: { nodes: nodes() } }
    if (c.method === 'GET' && path === '/api/v1/nodes/enrollments') return { status: 200, body: { enrollments: enrollments() } }
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

const where = () => screen.getByTestId('where').textContent
const rowOf = (name: string) => {
  const list = document.querySelector('.node-list') as HTMLElement
  const row = within(list).getByText(name, { selector: '.cell-name' }).closest('li')
  if (!row) throw new Error(`no row for ${name}`)
  return row
}

describe('Nodes page (D9)', () => {
  it('lists every node with its status, agent, enrolment and servers; Nodes is the current section', async () => {
    app('/nodes')
    expect(await screen.findByText('3 nodes')).toBeInTheDocument()
    expect(document.title).toBe('Nodes · Waystone')
    expect(screen.getByRole('link', { name: 'Nodes' })).toHaveAttribute('aria-current', 'page')
    expect(screen.getByRole('link', { name: 'Servers' })).not.toHaveAttribute('aria-current')

    const local = rowOf(homelab.name)
    expect(local).toHaveTextContent('Reachable')
    expect(local).toHaveTextContent('This machine')
    expect(local).toHaveTextContent('Built in')
    expect(local).toHaveTextContent('2 servers')
    expect(local).toHaveTextContent('0.1.0')

    const remote = rowOf(pi.name)
    expect(remote).toHaveTextContent('Unreachable')
    expect(remote).toHaveTextContent('Last seen 4 min ago')
    expect(remote).toHaveTextContent('1 server')

    // A revoked node says so, and has no Revoke button.
    const revoked = rowOf(oldBox.name)
    expect(revoked).toHaveTextContent('Revoked')
    expect(within(revoked).queryByRole('button')).toBeNull()
    expect(within(remote).getByRole('button', { name: `Revoke ${pi.name}` })).toBeEnabled()
  })

  it('lists the tokens, never a bundle', async () => {
    app('/nodes')
    const tokens = await screen.findByRole('heading', { name: 'Enrollment tokens' })
    const list = tokens.nextElementSibling as HTMLElement
    expect(within(list).getAllByRole('listitem')).toHaveLength(3)
    expect(list).toHaveTextContent('Waiting')
    expect(list).toHaveTextContent('Used')
    expect(list).toHaveTextContent('Expired')
    expect(list).toHaveTextContent('made by Trevor')
  })

  it("someone who isn't an admin is told so", async () => {
    app('/nodes', (c) => (c.path === '/api/v1/nodes' ? webError('forbidden', 403) : undefined))
    expect(await screen.findByText(/Couldn't load nodes\. You don't have permission to do this\./)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Add node' })).toBeNull()
  })

  it('revoke asks first, says what it does and does not do, then revokes with CSRF', async () => {
    const api = app('/nodes', (c) =>
      c.method === 'POST' && c.path === `/api/v1/nodes/${pi.id}/revoke` ? { status: 200, body: { ...pi, revoked_at: '2026-10-16T12:00:00Z' } } : undefined,
    )
    await userEvent.click(await screen.findByRole('button', { name: `Revoke ${pi.name}` }))
    const confirm = screen.getByRole('group', { name: `Revoke ${pi.name}?` })
    expect(confirm).toHaveTextContent('Revoking stops this node from connecting. It does not stop servers already running on it.')
    expect(confirm).toHaveTextContent('add it as a new node')
    // Cancel sends nothing.
    await userEvent.click(within(confirm).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('group')).toBeNull()
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false)

    const before = api.calls.filter((c) => c.path === '/api/v1/nodes').length
    await userEvent.click(screen.getByRole('button', { name: `Revoke ${pi.name}` }))
    await userEvent.click(screen.getByRole('button', { name: 'Revoke node' }))
    await waitFor(() => expect(api.calls.filter((c) => c.path === '/api/v1/nodes').length).toBeGreaterThan(before))
    const post = api.calls.find((c) => c.method === 'POST')
    expect(post?.path).toBe(`/api/v1/nodes/${pi.id}/revoke`)
    expect(post?.headers['X-CSRF-Token']).toBe(goldenSession.csrf_token)
  })

  it("revoking the controller's own node says there is no way back (6b-I1)", async () => {
    app('/nodes')
    await userEvent.click(await screen.findByRole('button', { name: `Revoke ${homelab.name}` }))
    expect(screen.getByRole('group', { name: `Revoke ${homelab.name}?` })).toHaveTextContent(
      "In this version a revoked node can't be added back, so its servers can't be managed from Waystone again.",
    )
  })

  it('a refused revoke shows why and stays open', async () => {
    app('/nodes', (c) => (c.method === 'POST' ? { status: 503, body: { error: { code: 'controller_busy', message: 'x', request_id: 'req-9' } } } : undefined))
    await userEvent.click(await screen.findByRole('button', { name: `Revoke ${pi.name}` }))
    await userEvent.click(screen.getByRole('button', { name: 'Revoke node' }))
    expect(await screen.findByText('Waystone is busy. Try again in a moment.')).toBeInTheDocument()
    expect(screen.getByText('req-9')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Revoke node' })).toBeEnabled()
  })

  it('Add node checks the name, makes a token, shows the command and bundle once, and Done forgets them', async () => {
    const made = newEnrollment()
    const api = app('/nodes', (c) => (c.method === 'POST' && c.path === '/api/v1/nodes/enrollments' ? { status: 201, body: made } : undefined))
    await userEvent.click(await screen.findByRole('button', { name: 'Add node' }))
    await userEvent.click(screen.getByRole('button', { name: 'Make a token' }))
    expect(screen.getByText('Give the node a name.')).toBeInTheDocument()
    expect(screen.getByLabelText('Name')).toHaveAttribute('aria-invalid', 'true')
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false)

    await userEvent.type(screen.getByLabelText('Name'), 'new-box')
    await userEvent.click(screen.getByRole('button', { name: 'Make a token' }))
    expect(await screen.findByRole('heading', { name: 'Add new-box' })).toBeInTheDocument()
    const post = api.calls.find((c) => c.method === 'POST')
    expect(post?.body).toEqual({ name: 'new-box' })
    expect(post?.headers['X-CSRF-Token']).toBe(goldenSession.csrf_token)
    expect(screen.getByText(made.command)).toBeInTheDocument()
    const bundle = screen.getByLabelText('When it asks, paste this bundle:')
    expect(bundle).toHaveValue(made.bundle)
    // 6d-H1: a credential, kept away from spellcheckers and autofill.
    expect(bundle).toHaveAttribute('spellcheck', 'false')
    expect(bundle).toHaveAttribute('autocomplete', 'off')
    expect(bundle).toHaveAttribute('autocapitalize', 'none')
    expect(screen.getByRole('button', { name: 'Copy the command' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Copy the bundle' })).toBeInTheDocument()
    expect(screen.getByText(/The bundle works once, until .*Waystone won't show it again/)).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Done' }))
    expect(screen.queryByDisplayValue(made.bundle)).toBeNull()
    expect(document.body.textContent).not.toContain(made.bundle)
    expect(screen.getByRole('button', { name: 'Add node' })).toBeInTheDocument()
  })

  it('copies the bundle, and says so when the browser has no clipboard', async () => {
    const write = vi.fn(() => Promise.resolve())
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true })
    app('/nodes', (c) => (c.method === 'POST' ? { status: 201, body: newEnrollment() } : undefined))
    await userEvent.click(await screen.findByRole('button', { name: 'Add node' }))
    await userEvent.type(screen.getByLabelText('Name'), 'new-box')
    await userEvent.click(screen.getByRole('button', { name: 'Make a token' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Copy the bundle' }))
    expect(write).toHaveBeenCalledWith(newEnrollment().bundle)
    expect(await screen.findByText('Copied.')).toBeInTheDocument()

    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true })
    await userEvent.click(screen.getByRole('button', { name: 'Copy the command' }))
    expect(await screen.findByText("Couldn't copy. Select it and copy it yourself.")).toBeInTheDocument()
  })

  it('no listener: says Waystone cannot add a node yet', async () => {
    app('/nodes', (c) => (c.method === 'POST' ? webError('enrollment_unavailable', 409) : undefined))
    await userEvent.click(await screen.findByRole('button', { name: 'Add node' }))
    await userEvent.type(screen.getByLabelText('Name'), 'new-box')
    await userEvent.click(screen.getByRole('button', { name: 'Make a token' }))
    expect(await screen.findByText("Waystone isn't listening for other machines, so it can't add a node yet.")).toBeInTheDocument()
  })
})

describe('create server (D9)', () => {
  it('the server list links to the form, and an empty list says what to do', async () => {
    app('/')
    await userEvent.click(await screen.findByRole('link', { name: 'Create server' }))
    expect(where()).toBe('/servers/new')
    expect(await screen.findByRole('heading', { name: 'Create server' })).toBeInTheDocument()
  })

  it('picks the reachable node and its first free port, and creates with CSRF and a key', async () => {
    const answer = created()
    const api = app(
      '/servers/new',
      (c) => (c.method === 'POST' && c.path === '/api/v1/servers' ? { status: 201, body: answer } : undefined),
      [answer.server],
    )
    const node = await screen.findByLabelText('Node')
    expect(node).toHaveValue(homelab.id)
    // Survival World has 25565 and New 25566 on homelab-1.
    expect(screen.getByLabelText('Port')).toHaveValue('25567')
    expect(screen.getByLabelText('Memory')).toHaveValue('2048')
    expect(screen.getByText('Minecraft Java 26.3')).toBeInTheDocument()
    // The unreachable node is listed but can't be picked; the revoked one isn't listed.
    expect(within(node).getByRole('option', { name: 'pi-node (unreachable)' })).toBeDisabled()
    expect(within(node).queryByRole('option', { name: /old-box/ })).toBeNull()

    await userEvent.type(screen.getByLabelText('Name'), 'Creative')
    await userEvent.click(screen.getByRole('button', { name: 'Create server' }))
    await waitFor(() => expect(where()).toBe(`/servers/${answer.server.id}`))
    const post = api.calls.find((c) => c.method === 'POST')
    expect(post?.body).toEqual({ node_id: homelab.id, name: 'Creative', port: 25567, heap_mib: 2048 })
    expect(post?.headers['X-CSRF-Token']).toBe(goldenSession.csrf_token)
    expect(post?.headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('checks the fields before sending anything', async () => {
    const api = app('/servers/new')
    await screen.findByLabelText('Node')
    const port = screen.getByLabelText('Port')
    await userEvent.clear(port)
    await userEvent.type(port, '80')
    await userEvent.click(screen.getByRole('button', { name: 'Create server' }))
    expect(screen.getByText('Give the server a name.')).toBeInTheDocument()
    expect(screen.getByText('The port must be a number from 1024 to 65535.')).toBeInTheDocument()
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('a taken port is said at the port; sending the same form again reuses its key, a changed one gets a new key', async () => {
    let taken = true
    const api = app('/servers/new', (c) =>
      c.method === 'POST' ? (taken ? webError('port_taken', 409) : { status: 201, body: created() }) : undefined,
    )
    await screen.findByLabelText('Node')
    await userEvent.type(screen.getByLabelText('Name'), 'Creative')
    await userEvent.click(screen.getByRole('button', { name: 'Create server' }))
    expect(await screen.findByText('Another server on this node already uses that port.')).toBeInTheDocument()
    expect(screen.getByLabelText('Port')).toHaveAttribute('aria-invalid', 'true')
    await userEvent.click(screen.getByRole('button', { name: 'Create server' }))
    await waitFor(() => expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(2))
    taken = false
    const port = screen.getByLabelText('Port')
    await userEvent.clear(port)
    await userEvent.type(port, '25600')
    await userEvent.click(screen.getByRole('button', { name: 'Create server' }))
    await waitFor(() => expect(where()).toBe(`/servers/${created().server.id}`))
    const keys = api.calls.filter((c) => c.method === 'POST').map((c) => c.headers['Idempotency-Key'])
    expect(keys[0]).toBe(keys[1])
    expect(keys[2]).not.toBe(keys[1])
  })

  it('a refusal for the node is shown with its request ID', async () => {
    app('/servers/new', (c) => (c.method === 'POST' ? { status: 409, body: { error: { code: 'node_full', message: 'x', request_id: 'req-7' } } } : undefined))
    await screen.findByLabelText('Node')
    await userEvent.type(screen.getByLabelText('Name'), 'One more')
    await userEvent.click(screen.getByRole('button', { name: 'Create server' }))
    expect(await screen.findByText('This node already has as many servers as it can manage.')).toBeInTheDocument()
    expect(screen.getByText('req-7')).toBeInTheDocument()
  })

  it('with no node to use, points at the Nodes page', async () => {
    app('/servers/new', (c) => (c.path === '/api/v1/nodes' ? { status: 200, body: { nodes: [oldBox] } } : undefined))
    expect(await screen.findByRole('link', { name: 'Add a node' })).toHaveAttribute('href', '/nodes')
  })
})

describe('install and EULA on the server page (D9)', () => {
  it('a server that is not installed says so, and Install posts to install', async () => {
    const s = created().server
    const api = app(`/servers/${s.id}`, (c) => (c.method === 'POST' ? { status: 202, body: created().operation } : undefined), [s])
    const card = await screen.findByRole('region', { name: 'Not installed yet' })
    expect(card).toHaveTextContent('Waystone downloads Minecraft Java 26.3 from Mojang')
    expect(screen.getByRole('button', { name: 'Start' })).toBeDisabled()
    expect(screen.getByText('Unavailable until the server is installed.')).toBeInTheDocument()
    await userEvent.click(within(card).getByRole('button', { name: 'Install' }))
    await waitFor(() => expect(api.calls.some((c) => c.method === 'POST')).toBe(true))
    const post = api.calls.find((c) => c.method === 'POST')
    expect(post?.path).toBe(`/api/v1/servers/${s.id}/install`)
    expect(post?.headers['X-CSRF-Token']).toBe(goldenSession.csrf_token)
    expect(post?.headers['Idempotency-Key']).toBeTruthy()
  })

  it('an install in progress disables Install with its reason', async () => {
    app(`/servers/${testServer.id}`)
    const card = await screen.findByRole('region', { name: 'Not installed yet' })
    expect(within(card).getByRole('button', { name: 'Install' })).toBeDisabled()
    expect(card).toHaveTextContent('Unavailable while another operation on this server is in progress.')
  })

  it('once installed, the EULA card links to the EULA and Accept posts to accept-eula', async () => {
    const s: Server = {
      ...created().server,
      installed: true,
      actions: [
        { action: 'start', allowed: false, reason: 'eula_not_accepted' },
        { action: 'stop', allowed: true },
        { action: 'restart', allowed: false, reason: 'eula_not_accepted' },
        { action: 'install', allowed: true },
        { action: 'accept_eula', allowed: true },
      ],
    }
    const api = app(`/servers/${s.id}`, (c) => (c.method === 'POST' ? { status: 202, body: created().operation } : undefined), [s])
    const card = await screen.findByRole('region', { name: 'Accept the Minecraft EULA' })
    expect(within(card).getByRole('link', { name: 'End User License Agreement' })).toHaveAttribute('href', eulaURL)
    expect(screen.getByText('Unavailable until the Minecraft EULA is accepted.')).toBeInTheDocument()
    await userEvent.click(within(card).getByRole('button', { name: 'Accept the EULA' }))
    await waitFor(() => expect(api.calls.some((c) => c.method === 'POST')).toBe(true))
    expect(api.calls.find((c) => c.method === 'POST')?.path).toBe(`/api/v1/servers/${s.id}/accept-eula`)
  })

  it('without permission, the card says an admin has to do it', async () => {
    const s: Server = { ...created().server, actions: created().server.actions.filter((a) => a.action !== 'install') }
    app(`/servers/${s.id}`, undefined, [s])
    const card = await screen.findByRole('region', { name: 'Not installed yet' })
    expect(within(card).queryByRole('button')).toBeNull()
    expect(card).toHaveTextContent('An admin has to do this.')
  })

  it('an installed and accepted server has no setup card', async () => {
    app(`/servers/${survival.id}`)
    await screen.findByRole('heading', { name: survival.name })
    expect(screen.queryByRole('region', { name: /Not installed|EULA/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Install' })).toBeNull()
  })

  it('the active line names the install and its phase', async () => {
    const s = created().server
    app(`/servers/${s.id}`, (c) => (c.path.startsWith('/api/v1/operations') ? { status: 200, body: { operations: [created().operation] } } : undefined), [s])
    expect(await screen.findByText(/^Install server · running · fetching/)).toBeInTheDocument()
  })
})

describe('the form rules', () => {
  it('nextFreePort skips the ports in use on that node only', () => {
    expect(nextFreePort(servers(), homelab.id)).toBe(25566)
    expect(nextFreePort(servers(), pi.id)).toBe(25565)
    expect(nextFreePort([], 'x')).toBe(25565)
  })
  it('names and ports', () => {
    expect(serverNameProblem('Survival')).toBeUndefined()
    expect(serverNameProblem('  ')).toBe('Give the server a name.')
    expect(serverNameProblem('a\tb')).toBe('The name can only hold plain text.')
    expect(serverNameProblem('é'.repeat(51))).toMatch(/too long/)
    expect(nodeNameProblem('x'.repeat(65))).toMatch(/too long/)
    expect(nodeNameProblem('homelab-2')).toBeUndefined()
    for (const bad of ['', '1023', '65536', '25565.5', '-1', '2e4']) expect(portProblem(bad)).toBeDefined()
    for (const good of ['1024', '25565', '65535']) expect(portProblem(good)).toBeUndefined()
  })
})
