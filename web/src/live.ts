// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError } from './api'
import { usePoll } from './poll'
import { useSession } from './session'

// Live is data a page keeps fresh (W5). After the first answer, a failed
// refresh keeps the last data and sets error, so the page can say it
// couldn't refresh without throwing away what it knows.
export interface Live<T> {
  data: T | undefined
  error: ApiError | undefined
  // at is when data was last loaded.
  at: Date | undefined
  reload(): Promise<void>
}

// useLive loads with load() at once and every 5 s while the tab is
// visible. A 401 means the session ended: the app goes to the login page
// and comes back here afterwards (scope §7, C-W8). A page that shows
// something else (another server) is a new component, keyed by its id.
export function useLive<T>(load: () => Promise<T>): Live<T> {
  const { expired } = useSession()
  const [state, setState] = useState<{ data: T | undefined; error: ApiError | undefined; at: Date | undefined }>({
    data: undefined,
    error: undefined,
    at: undefined,
  })
  const latest = useRef(load)
  useEffect(() => {
    latest.current = load
  })

  const reload = useCallback(async () => {
    try {
      const data = await latest.current()
      setState({ data, error: undefined, at: new Date() })
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        expired()
        return
      }
      const error = e instanceof ApiError ? e : new ApiError(0, 'network', String(e))
      setState((s) => ({ ...s, error }))
    }
  }, [expired])

  usePoll(reload)
  return { ...state, reload }
}
