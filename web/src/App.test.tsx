// SPDX-License-Identifier: AGPL-3.0-only

import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation } from 'react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'
import type { Client } from './api'
import { SessionProvider } from './session'
import { fakeClient, goldenSession, webError, type Call, type Reply } from './test/fakeApi'

function Where() {
  const l = useLocation()
  return <output data-testid="where">{l.pathname + l.search}</output>
}

function renderApp(client: Client, at = '/') {
  return render(
    <MemoryRouter initialEntries={[at]}>
      <SessionProvider client={client}>
        <App />
        <Where />
      </SessionProvider>
    </MemoryRouter>,
  )
}

// A tiny controller: one account, trevor / right, with the golden
// session's settings.
function controller(overrides: (c: Call) => Reply | Promise<Reply> | undefined = () => undefined, startSignedIn = false) {
  let signedIn = startSignedIn
  let prefs = { theme: 'slate', density: 'compact', mode: 'system' }
  return fakeClient((c) => {
    const o = overrides(c)
    if (o) return o
    const key = `${c.method} ${c.path}`
    switch (key) {
      case 'GET /api/v1/session':
        return signedIn ? { status: 200, body: { ...goldenSession, preferences: prefs } } : webError('unauthenticated', 401)
      case 'POST /api/v1/session': {
        const b = c.body as { username: string; password: string }
        if (b.username === 'trevor' && b.password === 'right') {
          signedIn = true
          return { status: 204 }
        }
        return webError('invalid_credentials', 401)
      }
      case 'DELETE /api/v1/session':
        signedIn = false
        return { status: 204 }
      case 'PATCH /api/v1/session/preferences':
        prefs = c.body as typeof prefs
        return { status: 200, body: prefs }
    }
    return webError('not_found', 404)
  })
}

afterEach(() => {
  vi.useRealTimers()
})

describe('signing in', () => {
  it('sends a signed-out visitor to the login page with next, and back after sign-in', async () => {
    const { client } = controller()
    renderApp(client, '/servers/s1?tab=x')
    await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent('/login?next=%2Fservers%2Fs1%3Ftab%3Dx'))
    await userEvent.type(screen.getByLabelText('Username'), 'trevor')
    await userEvent.type(screen.getByLabelText('Password'), 'right')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent('/servers/s1?tab=x'))
    expect(screen.getByRole('banner')).toHaveTextContent('Trevor')
  })

  it('never follows an unsafe next', async () => {
    const { client } = controller()
    renderApp(client, '/login?next=%2F%2Fevil.test')
    await userEvent.type(await screen.findByLabelText('Username'), 'trevor')
    await userEvent.type(screen.getByLabelText('Password'), 'right')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent(/^\/$/))
  })

  it('applies the account settings from the session, and caches them', async () => {
    const { client } = controller()
    renderApp(client, '/login')
    await userEvent.type(await screen.findByLabelText('Username'), 'trevor')
    await userEvent.type(screen.getByLabelText('Password'), 'right')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(document.documentElement.dataset.theme).toBe('slate'))
    expect(document.documentElement.dataset.density).toBe('compact')
    expect(JSON.parse(localStorage.getItem('waystone.appearance') ?? '{}')).toEqual({ theme: 'slate', density: 'compact', mode: 'system' })
  })

  it('says the same thing for any wrong username or password, and clears the password', async () => {
    const { client, calls } = controller()
    renderApp(client, '/login')
    await userEvent.type(await screen.findByLabelText('Username'), 'nobody')
    await userEvent.type(screen.getByLabelText('Password'), 'wrong')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByText('Username or password is wrong.')).toBeInTheDocument()
    expect(screen.getByLabelText('Password')).toHaveValue('')
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1)
  })

  it('too many attempts: says when, and the button waits until then', async () => {
    const { client, calls } = controller((c) =>
      c.method === 'POST' ? { ...webError('too_many_requests', 429), headers: { 'Retry-After': '120' } } : undefined,
    )
    renderApp(client, '/login')
    await userEvent.type(await screen.findByLabelText('Username'), 'trevor')
    await userEvent.type(screen.getByLabelText('Password'), 'right')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByText('Too many attempts. Try again in 2 minutes.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeDisabled()
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1)
  })

  it('a controller that cannot answer is not a wrong password', async () => {
    const { client } = controller((c) => (c.method === 'POST' ? webError('controller_busy', 503) : undefined))
    renderApp(client, '/login')
    await userEvent.type(await screen.findByLabelText('Username'), 'trevor')
    await userEvent.type(screen.getByLabelText('Password'), 'right')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByText("Waystone couldn't check your sign-in. Try again.")).toBeInTheDocument()
  })
})

async function signedIn(over?: (c: Call) => Reply | Promise<Reply> | undefined) {
  const api = controller(over)
  renderApp(api.client, '/login')
  await userEvent.type(await screen.findByLabelText('Username'), 'trevor')
  await userEvent.type(screen.getByLabelText('Password'), 'right')
  await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
  await screen.findByRole('banner')
  return api
}

describe('the frame and user menu', () => {
  it('has Servers and Nodes (D9) and a keyboard-reachable user menu', async () => {
    await signedIn()
    const nav = screen.getByRole('navigation', { name: 'Main' })
    expect(within(nav).getAllByRole('link').map((l) => l.textContent)).toEqual(['Servers', 'Nodes'])
    const button = screen.getByRole('button', { name: /Trevor/ })
    expect(button).toHaveAttribute('aria-expanded', 'false')
    button.focus()
    await userEvent.keyboard('{Enter}')
    expect(button).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByRole('group', { name: 'Theme' })).toBeVisible()
    await userEvent.keyboard('{Escape}')
    expect(button).toHaveAttribute('aria-expanded', 'false')
    expect(button).toHaveFocus()
  })

  it('Appearance sits above Sign out', async () => {
    await signedIn()
    await userEvent.click(screen.getByRole('button', { name: /Trevor/ }))
    const appearance = screen.getByRole('region', { name: 'Appearance' })
    const signOut = screen.getByRole('button', { name: 'Sign out' })
    expect(appearance.compareDocumentPosition(signOut) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('changing a setting applies it at once and saves it to the account', async () => {
    let release: () => void = () => {}
    const { calls } = await signedIn((c) =>
      c.method === 'PATCH' ? new Promise<Reply>((r) => (release = () => r({ status: 200, body: c.body }))) : undefined,
    )
    await userEvent.click(screen.getByRole('button', { name: /Trevor/ }))
    await userEvent.click(within(screen.getByRole('group', { name: 'Mode' })).getByLabelText('Light'))
    // Shown at once, while the save is still on its way.
    expect(document.documentElement.dataset.mode).toBe('light')
    expect(within(screen.getByRole('group', { name: 'Mode' })).getByLabelText('Light')).toBeChecked()
    const patch = calls.find((c) => c.method === 'PATCH')
    expect(patch).toMatchObject({ body: { theme: 'slate', density: 'compact', mode: 'light' }, headers: { 'X-CSRF-Token': goldenSession.csrf_token } })
    await act(async () => release())
    expect(within(screen.getByRole('group', { name: 'Mode' })).getByLabelText('Light')).toBeChecked()
    expect(JSON.parse(localStorage.getItem('waystone.appearance') ?? '{}')).toMatchObject({ mode: 'light' })
  })

  it('a setting that does not save goes back, and says so', async () => {
    await signedIn((c) => (c.method === 'PATCH' ? webError('controller_busy', 503) : undefined))
    await userEvent.click(screen.getByRole('button', { name: /Trevor/ }))
    await userEvent.click(within(screen.getByRole('group', { name: 'Theme' })).getByLabelText('Stone'))
    expect(await screen.findByText(/Couldn't save the appearance settings/)).toBeInTheDocument()
    expect(document.documentElement.dataset.theme).toBe('slate')
    expect(within(screen.getByRole('group', { name: 'Theme' })).getByLabelText('Slate')).toBeChecked()
  })

  it('sign out ends the session and shows the login page with a note', async () => {
    const { calls } = await signedIn()
    await userEvent.click(screen.getByRole('button', { name: /Trevor/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Sign out' }))
    expect(await screen.findByText("You've been signed out.")).toBeInTheDocument()
    expect(calls.find((c) => c.method === 'DELETE')?.headers['X-CSRF-Token']).toBe(goldenSession.csrf_token)
  })

  it('a sign out that fails keeps you signed in, and says so', async () => {
    await signedIn((c) => (c.method === 'DELETE' ? webError('controller_busy', 503) : undefined))
    await userEvent.click(screen.getByRole('button', { name: /Trevor/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Sign out' }))
    expect(await screen.findByText("Couldn't sign out. Try again.")).toBeInTheDocument()
    expect(screen.getByRole('banner')).toBeInTheDocument()
  })
})

it('shows "Couldn\'t reach Waystone" when the session check has no answer, and retries', async () => {
  let fail = true
  const { client } = controller((c) => (c.path === '/api/v1/session' && c.method === 'GET' && fail ? webError('controller_busy', 503) : undefined))
  renderApp(client, '/')
  expect(await screen.findByText("Couldn't reach Waystone.")).toBeInTheDocument()
  fail = false
  await act(async () => userEvent.click(screen.getByRole('button', { name: 'Try again' })))
  await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent('/login'))
})

it('an unknown address is a page inside the frame, not a blank screen', async () => {
  const { client } = controller(undefined, true)
  renderApp(client, '/nope')
  expect(await screen.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
  expect(screen.getByRole('banner')).toBeInTheDocument()
})

it('a signed-in visitor to the login page goes on to next', async () => {
  const { client } = controller(undefined, true)
  renderApp(client, '/login?next=%2Foperations%2Fo1')
  await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent('/operations/o1'))
})
