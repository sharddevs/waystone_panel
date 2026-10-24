// SPDX-License-Identifier: AGPL-3.0-only

import { expect, it } from 'vitest'
import { apply, applyCached, defaultPreferences, parsePreferences, readCached, writeCached } from './appearance'

it('the defaults are Stone, comfortable, dark', () => {
  expect(defaultPreferences).toEqual({ theme: 'stone', density: 'comfortable', mode: 'dark' })
})

it('applies the three attributes', () => {
  apply({ theme: 'slate', density: 'compact', mode: 'system' })
  const d = document.documentElement.dataset
  expect([d.theme, d.density, d.mode]).toEqual(['slate', 'compact', 'system'])
})

it('applies the cached settings before anything renders', () => {
  writeCached({ theme: 'slate', density: 'compact', mode: 'light' })
  apply(defaultPreferences)
  expect(applyCached()).toEqual({ theme: 'slate', density: 'compact', mode: 'light' })
  expect(document.documentElement.dataset.mode).toBe('light')
})

it('falls back to the defaults for a missing, broken or wrong cache', () => {
  expect(readCached()).toEqual(defaultPreferences)
  localStorage.setItem('waystone.appearance', '{not json')
  expect(readCached()).toEqual(defaultPreferences)
  localStorage.setItem('waystone.appearance', JSON.stringify({ theme: 'neon', density: 'compact', mode: 'dark' }))
  expect(readCached()).toEqual(defaultPreferences)
  const throwing = { getItem: () => { throw new Error('denied') } } as unknown as Storage
  expect(readCached(throwing)).toEqual(defaultPreferences)
})

it('parsePreferences keeps only the three fields', () => {
  expect(parsePreferences({ theme: 'stone', density: 'compact', mode: 'dark', x: 1 })).toEqual({ theme: 'stone', density: 'compact', mode: 'dark' })
  expect(parsePreferences({ theme: 'stone', density: 'compact' })).toBeUndefined()
  expect(parsePreferences(null)).toBeUndefined()
})
