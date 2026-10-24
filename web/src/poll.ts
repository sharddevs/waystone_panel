// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useRef } from 'react'

// pollInterval is W5's refresh: every 5 s while the tab is visible.
export const pollInterval = 5000

// usePoll calls fn at once and then every interval ms, but only while the
// document is visible. Hiding the tab stops it; showing it again calls fn
// at once and starts the timer again. A call never overlaps the previous
// one: the next wait starts when the previous call has finished.
export function usePoll(fn: () => unknown, interval: number = pollInterval): void {
  const latest = useRef(fn)
  useEffect(() => {
    latest.current = fn
  })

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined
    let running = false
    let stopped = false

    const schedule = () => {
      clearTimeout(timer)
      timer = undefined
      if (!stopped && document.visibilityState === 'visible') timer = setTimeout(tick, interval)
    }
    const tick = async () => {
      if (stopped || running || document.visibilityState !== 'visible') return
      running = true
      try {
        await latest.current()
      } catch {
        // The caller shows its own errors; polling carries on.
      } finally {
        running = false
        schedule()
      }
    }
    const onVisibility = () => {
      if (document.visibilityState === 'visible') void tick()
      else {
        clearTimeout(timer)
        timer = undefined
      }
    }

    document.addEventListener('visibilitychange', onVisibility)
    void tick()
    return () => {
      stopped = true
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [interval])
}
