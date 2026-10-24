// SPDX-License-Identifier: AGPL-3.0-only

import { act, render } from '@testing-library/react'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { pollInterval, usePoll } from './poll'

let visibility: DocumentVisibilityState = 'visible'

beforeEach(() => {
  vi.useFakeTimers()
  visibility = 'visible'
  vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visibility)
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

function setVisibility(v: DocumentVisibilityState) {
  visibility = v
  document.dispatchEvent(new Event('visibilitychange'))
}

function Poller({ fn }: { fn: () => unknown }) {
  usePoll(fn)
  return null
}

it('is every 5 s (W5)', () => {
  expect(pollInterval).toBe(5000)
})

it('calls at once, then every interval while visible', async () => {
  const fn = vi.fn()
  render(<Poller fn={fn} />)
  await act(async () => {})
  expect(fn).toHaveBeenCalledTimes(1)
  await act(async () => vi.advanceTimersByTimeAsync(5000))
  expect(fn).toHaveBeenCalledTimes(2)
  await act(async () => vi.advanceTimersByTimeAsync(10000))
  expect(fn).toHaveBeenCalledTimes(4)
})

it('stops while hidden and refreshes at once when shown again', async () => {
  const fn = vi.fn()
  render(<Poller fn={fn} />)
  await act(async () => {})
  act(() => setVisibility('hidden'))
  await act(async () => vi.advanceTimersByTimeAsync(60000))
  expect(fn).toHaveBeenCalledTimes(1)
  await act(async () => setVisibility('visible'))
  expect(fn).toHaveBeenCalledTimes(2)
  await act(async () => vi.advanceTimersByTimeAsync(5000))
  expect(fn).toHaveBeenCalledTimes(3)
})

it('never overlaps a slow call, and survives a failing one', async () => {
  let resolve: () => void = () => {}
  const fn = vi.fn(() => new Promise<void>((r) => (resolve = r)))
  render(<Poller fn={fn} />)
  await act(async () => {})
  await act(async () => vi.advanceTimersByTimeAsync(20000))
  expect(fn).toHaveBeenCalledTimes(1)
  await act(async () => resolve())
  await act(async () => vi.advanceTimersByTimeAsync(5000))
  expect(fn).toHaveBeenCalledTimes(2)

  const failing = vi.fn(() => Promise.reject(new Error('down')))
  render(<Poller fn={failing} />)
  await act(async () => vi.advanceTimersByTimeAsync(5000))
  expect(failing).toHaveBeenCalledTimes(2)
})

it('does not call while mounted hidden, and calls once shown', async () => {
  visibility = 'hidden'
  const fn = vi.fn()
  render(<Poller fn={fn} />)
  await act(async () => vi.advanceTimersByTimeAsync(20000))
  expect(fn).toHaveBeenCalledTimes(0)
  await act(async () => setVisibility('visible'))
  expect(fn).toHaveBeenCalledTimes(1)
})

it('does not start a second call when shown again while one is still running', async () => {
  let resolve: () => void = () => {}
  const fn = vi.fn(() => new Promise<void>((r) => (resolve = r)))
  render(<Poller fn={fn} />)
  await act(async () => {})
  act(() => setVisibility('hidden'))
  await act(async () => setVisibility('visible'))
  expect(fn).toHaveBeenCalledTimes(1)
  await act(async () => resolve())
  await act(async () => vi.advanceTimersByTimeAsync(5000))
  expect(fn).toHaveBeenCalledTimes(2)
})

it('stops when unmounted', async () => {
  const fn = vi.fn()
  const { unmount } = render(<Poller fn={fn} />)
  await act(async () => {})
  unmount()
  await act(async () => vi.advanceTimersByTimeAsync(20000))
  expect(fn).toHaveBeenCalledTimes(1)
})
