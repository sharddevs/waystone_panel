// SPDX-License-Identifier: AGPL-3.0-only

// Appearance settings: theme, density and mode (design-tokens-v0.1.md).
// They are saved per account (PATCH /api/v1/session/preferences) and
// cached in localStorage, so the next page load can apply them before
// React renders anything (no flash of the wrong theme; scope §6).

export const themes = ['stone', 'slate'] as const
export const densities = ['comfortable', 'compact'] as const
export const modes = ['dark', 'light', 'system'] as const

export type Theme = (typeof themes)[number]
export type Density = (typeof densities)[number]
export type Mode = (typeof modes)[number]

export interface Preferences {
  theme: Theme
  density: Density
  mode: Mode
}

// The defaults match store.DefaultPreferences and index.html.
export const defaultPreferences: Preferences = { theme: 'stone', density: 'comfortable', mode: 'dark' }

const cacheKey = 'waystone.appearance'

function oneOf<T extends string>(values: readonly T[], v: unknown): v is T {
  return typeof v === 'string' && (values as readonly string[]).includes(v)
}

// parsePreferences returns p if it is exactly a valid Preferences, else
// undefined.
export function parsePreferences(p: unknown): Preferences | undefined {
  if (typeof p !== 'object' || p === null) return undefined
  const o = p as Record<string, unknown>
  if (!oneOf(themes, o.theme) || !oneOf(densities, o.density) || !oneOf(modes, o.mode)) return undefined
  return { theme: o.theme, density: o.density, mode: o.mode }
}

// readCached returns the cached settings, or the defaults when there are
// none or they don't parse. Storage can throw (disabled, private mode).
export function readCached(storage: Storage | undefined = globalThis.localStorage): Preferences {
  try {
    const raw = storage?.getItem(cacheKey)
    if (raw) return parsePreferences(JSON.parse(raw)) ?? defaultPreferences
  } catch {
    // Fall back to the defaults.
  }
  return defaultPreferences
}

export function writeCached(p: Preferences, storage: Storage | undefined = globalThis.localStorage): void {
  try {
    storage?.setItem(cacheKey, JSON.stringify(p))
  } catch {
    // Not cached: the next load starts from the defaults, then the session.
  }
}

// apply sets the three attributes tokens.css selects on.
export function apply(p: Preferences, root: HTMLElement = document.documentElement): void {
  root.dataset.theme = p.theme
  root.dataset.density = p.density
  root.dataset.mode = p.mode
}

// applyCached is the first thing main.tsx does.
export function applyCached(): Preferences {
  const p = readCached()
  apply(p)
  return p
}
