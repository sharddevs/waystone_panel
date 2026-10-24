// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { ApiError, client as defaultClient, type Client, type Session } from './api'
import { apply, writeCached, type Preferences } from './appearance'

// The signed-in session for the whole app. "loading" until GET
// /api/v1/session has answered once; "signed-out" after a 401; "error"
// when Waystone couldn't be asked at all.
export type SessionState =
  | { status: 'loading' }
  | { status: 'signed-in'; session: Session }
  | { status: 'signed-out'; reason?: 'signed-out' | 'expired' }
  | { status: 'error'; error: ApiError }

interface SessionApi {
  state: SessionState
  client: Client
  // refresh asks the controller who is signed in, and applies their
  // appearance settings (the server's copy wins over the cache).
  refresh(): Promise<void>
  signOut(): Promise<void>
  // expired is for any 401 from another route: the session ended.
  expired(): void
  setPreferences(p: Preferences): Promise<void>
}

const SessionContext = createContext<SessionApi | null>(null)

export function useSession(): SessionApi {
  const s = useContext(SessionContext)
  if (s === null) throw new Error('useSession outside SessionProvider')
  return s
}

function adopt(p: Preferences) {
  apply(p)
  writeCached(p)
}

export function SessionProvider({ client = defaultClient, children }: { client?: Client; children: ReactNode }) {
  const [state, setState] = useState<SessionState>({ status: 'loading' })

  // load asks who is signed in and applies their settings; it returns
  // the new state instead of setting it, so the first load can be
  // dropped if the provider unmounts first.
  const load = useCallback(async (): Promise<SessionState> => {
    try {
      const session = await client.session()
      adopt(session.preferences)
      return { status: 'signed-in', session }
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) return { status: 'signed-out' }
      return { status: 'error', error: e instanceof ApiError ? e : new ApiError(0, 'network', String(e)) }
    }
  }, [client])

  const refresh = useCallback(async () => setState(await load()), [load])

  useEffect(() => {
    let live = true
    void load().then((s) => {
      if (live) setState(s)
    })
    return () => {
      live = false
    }
  }, [load])

  const api = useMemo<SessionApi>(() => {
    const csrf = () => (state.status === 'signed-in' ? state.session.csrf_token : undefined)
    return {
      state,
      client,
      refresh,
      async signOut() {
        const token = csrf()
        if (token !== undefined) {
          try {
            await client.signOut(token)
          } catch (e) {
            // Already gone is signed out too; anything else, the user
            // must not believe they are signed out when they aren't.
            if (!(e instanceof ApiError && e.status === 401)) throw e
          }
        }
        setState({ status: 'signed-out', reason: 'signed-out' })
      },
      expired() {
        setState({ status: 'signed-out', reason: 'expired' })
      },
      async setPreferences(p) {
        const token = csrf()
        if (token === undefined || state.status !== 'signed-in') return
        // Shown at once (the page and the menu), saved after; a failed
        // save puts the previous settings back.
        const session = state.session
        const with_ = (preferences: Preferences): SessionState => ({ status: 'signed-in', session: { ...session, preferences } })
        adopt(p)
        setState(with_(p))
        try {
          const saved = await client.setPreferences(p, token)
          adopt(saved)
          setState(with_(saved))
        } catch (e) {
          adopt(session.preferences)
          setState(e instanceof ApiError && e.status === 401 ? { status: 'signed-out', reason: 'expired' } : with_(session.preferences))
          throw e
        }
      },
    }
  }, [state, client, refresh])

  return <SessionContext.Provider value={api}>{children}</SessionContext.Provider>
}

// useSignedIn is for pages behind RequireSession: the session is there.
export function useSignedIn(): { session: Session; client: Client; expired(): void } {
  const s = useSession()
  if (s.state.status !== 'signed-in') throw new Error('useSignedIn outside RequireSession')
  return { session: s.state.session, client: s.client, expired: s.expired }
}
