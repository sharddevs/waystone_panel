// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect } from 'react'
import { Link } from 'react-router'
import { StatusLine } from '../components/StatusMark'
import { errorText, gameLine } from '../format'
import { useLive } from '../live'
import { useSignedIn } from '../session'
import { summarize } from '../status'
import { Problem } from './Problem'

// The server list, the landing page (wireframes §2, W1). The whole row
// is the link; the only button is Create server (D9).
export function ServerList() {
  const { client } = useSignedIn()
  const live = useLive(() => client.servers())
  const now = live.at ?? new Date()

  useEffect(() => {
    document.title = 'Servers · Waystone'
  }, [])

  const servers = live.data ? [...live.data.servers].sort((a, b) => a.name.localeCompare(b.name)) : undefined
  const count = servers?.length

  return (
    <>
      <div className="page-head">
        <h1 className="page-title">Servers</h1>
        <div className="head-side">
          {count !== undefined && <p className="meta">{count === 1 ? '1 server' : `${count} servers`}</p>}
          <Link className="button primary" to="/servers/new">
            Create server
          </Link>
        </div>
      </div>

      {servers === undefined && live.error && (
        <Problem error={live.error} retry={() => void live.reload()}>
          Couldn't load servers.
        </Problem>
      )}

      {servers === undefined && !live.error && (
        <ul className="server-list" aria-busy="true" aria-label="Loading servers">
          {[0, 1, 2].map((i) => (
            <li key={i} className="server-row placeholder" aria-hidden="true">
              <span className="bone" />
            </li>
          ))}
        </ul>
      )}

      {servers !== undefined && servers.length === 0 && (
        <p className="empty">No servers yet. Create one, and Waystone installs it on the node you pick.</p>
      )}

      {servers !== undefined && servers.length > 0 && (
        <>
          <div className="server-head" aria-hidden="true">
            <span>Status</span>
            <span>Name</span>
            <span>Game</span>
            <span>Node</span>
          </div>
          <ul className="server-list">
            {servers.map((s) => {
              const sum = summarize(s, now)
              const why = sum.mark === 'unknown' ? sum.detail : undefined
              return (
                <li key={s.id}>
                  <Link to={`/servers/${encodeURIComponent(s.id)}`} className="server-row">
                    <span className="cell-status">
                      <StatusLine symbol={sum.mark} tone={sum.tone}>
                        {sum.title}
                      </StatusLine>
                      {why && <span className="why">{why}</span>}
                    </span>
                    <span className="cell-name">{s.name}</span>
                    <span className="cell-game">{gameLine(s)}</span>
                    <span className="cell-node">{s.node.name}</span>
                    <span className="chevron" aria-hidden="true">
                      ›
                    </span>
                  </Link>
                </li>
              )
            })}
          </ul>
          {live.error && <p className="meta stale">Couldn't refresh: {errorText(live.error)} Showing what Waystone last reported.</p>}
        </>
      )}
    </>
  )
}
