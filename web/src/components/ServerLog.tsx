// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { ApiError, type LogLine } from '../api'
import { cleanLine, errorText } from '../format'
import { Problem } from '../pages/Problem'
import { usePoll } from '../poll'
import { useSignedIn } from '../session'

// The server's log (7a, scope D1): the newest lines, then every few
// seconds the lines after the last one, while the tab is visible. Lines
// are plain text, newest at the bottom. Nothing is stored: a reload reads
// the newest lines from the node again.

// logPollInterval is how often the log is read.
export const logPollInterval = 3000
// keptLines is how many lines the page holds; older ones drop off.
export const keptLines = 1000
// readsPerPoll is how many reads one poll makes to catch up when the
// node says more lines are waiting.
const readsPerPoll = 5

// cleanLine moved to format.ts in 7b; the operation history shows command
// lines with it too.
export { cleanLine }

// lineTone colours errors and warnings by their syslog priority.
export function lineTone(priority: number): string {
  if (priority <= 3) return 'log-line log-bad'
  if (priority === 4) return 'log-line log-warn'
  return 'log-line'
}

function clock(t: string): string {
  return new Date(t).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
}

interface Shown {
  key: number
  line: LogLine
}

// children go under the log: the command box (7b).
export function ServerLog({ serverId, children }: { serverId: string; children?: ReactNode }) {
  const { client, expired } = useSignedIn()
  const [lines, setLines] = useState<Shown[] | undefined>(undefined)
  const [error, setError] = useState<ApiError | undefined>(undefined)
  const [follow, setFollow] = useState(true)
  const box = useRef<HTMLDivElement>(null)
  // cursor is where the next read continues; null reads the newest
  // lines and replaces what's shown. A failed read goes back to null, so
  // a cursor the node no longer knows can't get stuck.
  const cursor = useRef<string | null>(null)
  const nextKey = useRef(0)

  const read = useCallback(async () => {
    try {
      const fromTail = cursor.current === null
      let page = await client.logs(serverId, cursor.current)
      const got = [...page.lines]
      for (let i = 1; !fromTail && page.more && page.cursor !== null && i < readsPerPoll; i++) {
        page = await client.logs(serverId, page.cursor)
        got.push(...page.lines)
      }
      cursor.current = page.cursor
      const shown = got.map((line) => ({ key: nextKey.current++, line }))
      setLines((prev) => (fromTail ? shown : [...(prev ?? []), ...shown]).slice(-keptLines))
      setError(undefined)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        expired()
        return
      }
      cursor.current = null
      setError(e instanceof ApiError ? e : new ApiError(0, 'network', String(e)))
    }
  }, [client, serverId, expired])

  usePoll(read, logPollInterval)

  useLayoutEffect(() => {
    const el = box.current
    if (follow && el) el.scrollTop = el.scrollHeight
  }, [lines, follow])

  const onScroll = () => {
    const el = box.current
    if (el) setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 8)
  }

  return (
    <section className="server-log" aria-labelledby="log-title">
      <div className="log-head">
        <h2 id="log-title" className="section-title">
          Log
        </h2>
        {lines !== undefined && !follow && (
          <button type="button" className="button small" onClick={() => setFollow(true)}>
            Jump to newest
          </button>
        )}
      </div>
      {lines === undefined && error && (
        <Problem error={error} retry={() => void read()}>
          Couldn't read the log.
        </Problem>
      )}
      {lines === undefined && !error && <p className="meta">Loading…</p>}
      {lines !== undefined && (
        <div ref={box} className="log-box" role="region" aria-label="Server log" tabIndex={0} onScroll={onScroll}>
          {lines.length === 0 ? (
            <p className="meta">Nothing logged yet.</p>
          ) : (
            <ol className="log-lines">
              {lines.map(({ key, line }) => (
                <li key={key} className={lineTone(line.priority)}>
                  <time className="log-time" dateTime={line.time}>
                    {clock(line.time)}
                  </time>{' '}
                  <span className="log-text">{cleanLine(line.text)}</span>
                </li>
              ))}
            </ol>
          )}
        </div>
      )}
      {lines !== undefined && error && <p className="meta stale">Couldn't refresh the log: {errorText(error)}</p>}
      {children}
      <p className="meta">Read from the node each time; Waystone doesn't keep it. It can hold player names, addresses and chat.</p>
    </section>
  )
}
