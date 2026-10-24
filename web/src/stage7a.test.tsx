// SPDX-License-Identifier: AGPL-3.0-only

import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { readFileSync } from 'node:fs'
import { MemoryRouter, useLocation } from 'react-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'
import type { LogLine, LogPage, Operation, Server } from './api'
import { cleanLine, keptLines, lineTone } from './components/ServerLog'
import { SessionProvider } from './session'
import { fakeClient, goldenSession, webError, type Call, type Reply } from './test/fakeApi'

// Stage 7a (scope D1): the Logs section on the server page, against the
// Go side's goldens.

const golden = <T,>(f: string): T => JSON.parse(readFileSync(`../internal/view/testdata/${f}`, 'utf8')) as T
const [survival] = golden<{ servers: Server[] }>('servers.json').servers as [Server]
const logs = () => golden<LogPage>('logs.json')
const cursor = logs().cursor as string

function Where() {
  const l = useLocation()
  return <output data-testid="where">{l.pathname}</output>
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-10-16T12:00:00Z'))
})
afterEach(() => {
  vi.useRealTimers()
})

type LogHandler = (after: string | null, limit: string | null) => Reply | Promise<Reply>

function app(onLogs: LogHandler) {
  const api = fakeClient((c: Call) => {
    const url = new URL(c.path, 'http://x')
    if (c.method === 'GET' && url.pathname === '/api/v1/session') return { status: 200, body: goldenSession }
    if (c.method === 'GET' && url.pathname === `/api/v1/servers/${survival.id}/logs`) {
      return onLogs(url.searchParams.get('after'), url.searchParams.get('limit'))
    }
    if (c.method === 'GET' && url.pathname === `/api/v1/servers/${survival.id}`) return { status: 200, body: survival }
    if (c.method === 'GET' && url.pathname === '/api/v1/operations') return { status: 200, body: { operations: [] as Operation[] } }
    return webError('not_found', 404)
  })
  render(
    <MemoryRouter initialEntries={[`/servers/${survival.id}`]}>
      <SessionProvider client={api.client}>
        <App />
        <Where />
      </SessionProvider>
    </MemoryRouter>,
  )
  return api
}

const logCalls = (api: ReturnType<typeof app>) =>
  api.calls.filter((c) => c.path.includes('/logs')).map((c) => new URL(c.path, 'http://x').searchParams.get('after'))

// poll makes every live part of the page read again at once, the way
// coming back to the tab does.
async function poll() {
  await act(async () => {
    document.dispatchEvent(new Event('visibilitychange'))
    await Promise.resolve()
  })
}

const box = () => screen.getByRole('region', { name: 'Server log' })
const shownLines = () => within(box()).queryAllByRole('listitem').map((li) => li.querySelector('.log-text')?.textContent)

function line(text: string, priority = 6): LogLine {
  return { time: '2026-10-16T11:59:30Z', priority, text }
}

describe('Logs section (D1)', () => {
  it('shows the newest lines, oldest first, with their times', async () => {
    const api = app(() => ({ status: 200, body: logs() }))
    expect(await screen.findByRole('heading', { name: 'Log' })).toBeInTheDocument()
    await waitFor(() => expect(shownLines()).toHaveLength(logs().lines.length))
    expect(shownLines()).toEqual(logs().lines.map((l) => l.text))
    const first = within(box()).getAllByRole('listitem')[0] as HTMLElement
    expect(first.querySelector('time')).toHaveAttribute('dateTime', '2026-10-16T11:59:00Z')
    // The first read asks for the newest lines: no cursor, the default count.
    expect(logCalls(api)[0]).toBeNull()
    expect(api.calls.find((c) => c.path.includes('/logs'))?.path).toBe(`/api/v1/servers/${survival.id}/logs`)
    expect(screen.getByText(/Waystone doesn't keep it/)).toBeInTheDocument()
  })

  it('colours warnings and errors by priority', async () => {
    app(() => ({ status: 200, body: { lines: [line('ok'), line('careful', 4), line('broken', 3), line('worse', 0)], cursor, more: false } }))
    await waitFor(() => expect(shownLines()).toHaveLength(4))
    const items = within(box()).getAllByRole('listitem')
    expect(items.map((li) => li.className)).toEqual(['log-line', 'log-line log-warn', 'log-line log-bad', 'log-line log-bad'])
  })

  it('then reads only what came after, and appends it', async () => {
    let n = 0
    const api = app((after) => {
      n++
      if (after === null) return { status: 200, body: logs() }
      return { status: 200, body: { lines: [line(`new ${n}`)], cursor: `s=${n}`, more: false } }
    })
    await waitFor(() => expect(shownLines()).toHaveLength(logs().lines.length))
    await poll()
    await waitFor(() => expect(shownLines()).toHaveLength(logs().lines.length + 1))
    await poll()
    await waitFor(() => expect(shownLines()).toHaveLength(logs().lines.length + 2))
    const calls = logCalls(api)
    expect(calls[0]).toBeNull()
    expect(calls[1]).toBe(cursor)
    expect(calls[2]).toBe('s=2')
    expect(shownLines().slice(-2)).toEqual(['new 2', 'new 3'])
  })

  it('catches up when more lines are waiting, a few reads at a time', async () => {
    let n = 0
    const api = app((after) => {
      if (after === null) return { status: 200, body: { lines: [line('first')], cursor: 's=0', more: false } }
      n++
      return { status: 200, body: { lines: [line(`batch ${n}`)], cursor: `s=${n}`, more: true } }
    })
    await waitFor(() => expect(shownLines()).toEqual(['first']))
    await poll()
    await waitFor(() => expect(shownLines()).toHaveLength(6))
    expect(logCalls(api)).toEqual([null, 's=0', 's=1', 's=2', 's=3', 's=4'])
  })

  it('keeps at most the newest lines it can hold', async () => {
    const many = (from: number, count: number) => Array.from({ length: count }, (_, i) => line(`line ${from + i}`))
    app((after) => {
      if (after === null) return { status: 200, body: { lines: many(0, 200), cursor: 's=0', more: false } }
      const k = Number(after.slice(2))
      return { status: 200, body: { lines: many(200 * (k + 1), 200), cursor: `s=${k + 1}`, more: false } }
    })
    await waitFor(() => expect(shownLines()).toHaveLength(200))
    for (let i = 0; i < 5; i++) {
      await poll()
      await waitFor(() => expect(shownLines().at(-1)).toBe(`line ${200 * (i + 2) - 1}`))
    }
    expect(shownLines()).toHaveLength(keptLines)
    expect(shownLines()[0]).toBe('line 200')
  }, 30_000) // a thousand lines in jsdom, six times over

  it('says when nothing has been logged yet', async () => {
    app(() => ({ status: 200, body: golden<LogPage>('logs_empty.json') }))
    expect(await screen.findByText('Nothing logged yet.')).toBeInTheDocument()
  })

  it('a node that is away is said plainly, with a way to try again', async () => {
    let down = true
    app(() => (down ? webError('node_unreachable', 409) : { status: 200, body: logs() }))
    expect(await screen.findByText(/Couldn't read the log\. The node is unreachable/)).toBeInTheDocument()
    down = false
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(shownLines()).toHaveLength(logs().lines.length))
  })

  it('a failed refresh keeps the lines, says so, and starts again from the newest', async () => {
    let fail = false
    const api = app((after) => {
      if (fail) return webError('logs_unavailable', 503)
      if (after === null) return { status: 200, body: logs() }
      return { status: 200, body: { lines: [line('after')], cursor: 's=9', more: false } }
    })
    await waitFor(() => expect(shownLines()).toHaveLength(logs().lines.length))
    fail = true
    await poll()
    expect(await screen.findByText(/Couldn't refresh the log: The node couldn't read the server's log just now\./)).toBeInTheDocument()
    expect(shownLines()).toHaveLength(logs().lines.length)
    fail = false
    await poll()
    await waitFor(() => expect(screen.queryByText(/Couldn't refresh the log/)).toBeNull())
    // The read after a failure asks for the newest lines again, and
    // replaces what was shown.
    expect(logCalls(api).at(-1)).toBeNull()
    expect(shownLines()).toEqual(logs().lines.map((l) => l.text))
  })

  it('a session that ended goes to the sign-in page', async () => {
    app(() => webError('unauthorized', 401))
    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/login'))
  })

  it('stops following when scrolled up, and Jump to newest follows again', async () => {
    app(() => ({ status: 200, body: logs() }))
    await waitFor(() => expect(shownLines()).toHaveLength(logs().lines.length))
    const el = box()
    Object.defineProperty(el, 'scrollHeight', { configurable: true, value: 1000 })
    Object.defineProperty(el, 'clientHeight', { configurable: true, value: 300 })
    el.scrollTop = 100
    el.dispatchEvent(new Event('scroll'))
    const jump = await screen.findByRole('button', { name: 'Jump to newest' })
    await userEvent.click(jump)
    expect(el.scrollTop).toBe(1000)
    expect(screen.queryByRole('button', { name: 'Jump to newest' })).toBeNull()
  })

  it('the log box can be reached and scrolled with the keyboard', async () => {
    app(() => ({ status: 200, body: logs() }))
    await waitFor(() => expect(shownLines()).toHaveLength(logs().lines.length))
    expect(box()).toHaveAttribute('tabindex', '0')
  })
})

describe('cleanLine', () => {
  it('replaces control characters and bidi controls, keeps tabs and the rest', () => {
    const bad = String.fromCharCode(0x1b) + '[2J' + String.fromCharCode(0x202e) + 'x' + String.fromCharCode(0x85) + 'y\r\n'
    expect(cleanLine(bad)).toBe('\uFFFD[2J\uFFFDx\uFFFDy\uFFFD\uFFFD')
    expect(cleanLine('tab\there é 🎉')).toBe('tab\there é 🎉')
    for (const c of [0x0, 0x7, 0xb, 0x1f, 0x7f, 0x9f, 0x200e, 0x200f, 0x61c, 0x202a, 0x202d, 0x2066, 0x2069]) {
      expect(cleanLine(String.fromCharCode(c))).toBe('\uFFFD')
    }
    for (const c of [0x9, 0x20, 0xa0, 0x200d, 0x2070]) {
      expect(cleanLine(String.fromCharCode(c))).toBe(String.fromCharCode(c))
    }
  })
})

describe('lineTone', () => {
  it('maps syslog priorities', () => {
    expect([0, 1, 2, 3, 4, 5, 6, 7].map(lineTone)).toEqual([
      'log-line log-bad',
      'log-line log-bad',
      'log-line log-bad',
      'log-line log-bad',
      'log-line log-warn',
      'log-line',
      'log-line',
      'log-line',
    ])
  })
})
