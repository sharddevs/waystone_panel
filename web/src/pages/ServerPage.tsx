// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router'
import { ApiError, isServerAction, type Dimension, type Operation, type PageAction, type Server, type ServerAction } from '../api'
import { StatusLine } from '../components/StatusMark'
import { actionName, ago, errorText, formatTime, gameLine, isActive, outcome, stateMark, stateWords, took } from '../format'
import { useLive } from '../live'
import { useSignedIn } from '../session'
import { dimensionsLine, reasonText, summarize } from '../status'
import { Problem } from './Problem'

// historySteps are the history sizes "Show more" walks through; the API
// returns at most 100 (store.MaxListLimit).
const historySteps = [10, 50, 100]

const buttonWords: Record<PageAction, { idle: string; busy: string }> = {
  start: { idle: 'Start', busy: 'Starting…' },
  stop: { idle: 'Stop', busy: 'Stopping…' },
  restart: { idle: 'Restart', busy: 'Restarting…' },
  install: { idle: 'Install', busy: 'Installing…' },
  accept_eula: { idle: 'Accept the EULA', busy: 'Accepting…' },
}

// eulaURL is Mojang's EULA, the link a server's own eula.txt names.
export const eulaURL = 'https://aka.ms/MinecraftEULA'

export function ServerPageRoute() {
  const { id = '' } = useParams()
  return <ServerPage key={id} id={id} />
}

// The server page (wireframes §3, server page §1-§5).
export function ServerPage({ id }: { id: string }) {
  const { client, session, expired } = useSignedIn()
  const [step, setStep] = useState(0)
  const limit = historySteps[step] ?? 10
  const server = useLive(() => client.server(id))
  const history = useLive(() => client.operations({ serverId: id, limit }))
  const [pending, setPending] = useState<PageAction | null>(null)
  const [accepted, setAccepted] = useState<Operation | null>(null)
  const [actionError, setActionError] = useState<ApiError | null>(null)
  const { reload: reloadServer } = server
  const { reload: reloadHistory } = history

  const name = server.data?.name
  useEffect(() => {
    document.title = `${name ?? 'Server'} · Waystone`
  }, [name])

  useEffect(() => {
    if (step > 0) void reloadHistory()
  }, [step, reloadHistory])

  const act = useCallback(
    async (action: PageAction) => {
      setPending(action)
      setActionError(null)
      try {
        // One key per click: a retried request can't run twice.
        const op = await client.act(id, action, session.csrf_token, crypto.randomUUID())
        setAccepted(op)
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) {
          // The session ended: off to the login page, nothing to reload
          // (5d audit I12).
          expired()
          return
        }
        setActionError(e instanceof ApiError ? e : new ApiError(0, 'network', String(e)))
      } finally {
        setPending(null)
      }
      await Promise.all([reloadServer(), reloadHistory()])
    },
    [client, id, session.csrf_token, expired, reloadServer, reloadHistory],
  )

  if (!server.data) {
    return (
      <>
        <BackToServers />
        {server.error ? (
          <Problem error={server.error} retry={() => void server.reload()}>
            {server.error.code === 'not_found' ? "Couldn't find this server." : "Couldn't load this server."}
          </Problem>
        ) : (
          <p className="loading" role="status">
            Loading…
          </p>
        )}
      </>
    )
  }

  const s = server.data
  const now = server.at ?? new Date()
  const ops = history.data?.operations ?? []
  // The active line (R2) is the newest pending or running operation: the
  // one just accepted until the history has caught up with it.
  const active = ops.find(isActive) ?? (accepted && isActive(accepted) && !ops.some((o) => o.id === accepted.id) ? accepted : undefined)
  // F13: an unresolved uncertain operation blocks every action, whichever
  // reason the actions list names first (an unreachable node wins there).
  const uncertainOp = ops.find((o) => o.state === 'uncertain' && o.resolution === null)
  const blockedByUncertain = uncertainOp !== undefined || s.actions.some((a) => !a.allowed && a.reason === 'previous_operation_uncertain')

  return (
    <>
      <BackToServers />
      <header className="server-header">
        <div>
          <h1 className="page-title">{s.name}</h1>
          <p className="meta">
            on {s.node.name} · {s.runtime}
          </p>
        </div>
        <p className="game">{gameLine(s)}</p>
      </header>

      {server.error && <p className="meta stale">Couldn't refresh: {errorText(server.error)} Showing what Waystone last reported.</p>}

      <StatusCard server={s} now={now} />

      {blockedByUncertain && (
        <div className="callout" role="note">
          <p className="callout-title">The last operation's outcome is uncertain.</p>
          <p>Actions stay unavailable until an admin resolves it.</p>
          {uncertainOp && (
            <Link className="button" to={`/operations/${encodeURIComponent(uncertainOp.id)}`}>
              Review operation
            </Link>
          )}
        </div>
      )}

      <Setup server={s} pending={pending} onAct={(a) => void act(a)} />
      <Actions server={s} pending={pending} onAct={(a) => void act(a)} />
      <div aria-live="polite">
        {actionError && (
          <div className="problem-block">
            <p className="problem">{errorText(actionError)}</p>
            {actionError.operationId && (
              <Link to={`/operations/${encodeURIComponent(actionError.operationId)}`}>Open that operation</Link>
            )}
            {actionError.requestId && (
              <p className="meta">
                Request ID <span className="mono">{actionError.requestId}</span>
              </p>
            )}
          </div>
        )}
      </div>

      <div aria-live="polite">{active && <ActiveLine op={active} now={now} />}</div>

      <section className="history" aria-labelledby="history-title">
        <h2 id="history-title" className="section-title">
          Recent operations
        </h2>
        {history.data === undefined && history.error && (
          <Problem error={history.error} retry={() => void history.reload()}>
            Couldn't load the history.
          </Problem>
        )}
        {history.data === undefined && !history.error && <p className="meta">Loading…</p>}
        {history.data !== undefined && ops.length === 0 && <p className="meta">Nothing has been done to this server yet.</p>}
        {ops.length > 0 && (
          <ul className="history-list">
            {ops.map((o) => (
              <HistoryRow key={o.id} op={o} now={now} />
            ))}
          </ul>
        )}
        {ops.length >= limit && step < historySteps.length - 1 && (
          <button type="button" className="button" onClick={() => setStep(step + 1)}>
            Show more
          </button>
        )}
      </section>
    </>
  )
}

function BackToServers() {
  return (
    <p className="back">
      <Link to="/">← Servers</Link>
    </p>
  )
}

// StatusCard is the summary (§2), the three dimensions in words, the last
// check, and the raw facts behind "Details".
function StatusCard({ server: s, now }: { server: Server; now: Date }) {
  const sum = summarize(s, now)
  const checked = ago(s.process.observed_at, now)
  return (
    <section className="card status-card" aria-label="Status">
      <p className="summary" aria-live="polite">
        <StatusLine symbol={sum.mark} tone={sum.tone}>
          {sum.title}
        </StatusLine>
      </p>
      <p className="detail">{sum.detail}</p>
      <p className="dims">{dimensionsLine(s, now)}</p>
      <div className="card-foot">
        <span className="meta">{checked ? `Checked ${checked}` : 'Not checked yet'}</span>
      </div>
      <details className="raw">
        <summary>Details</summary>
        <dl className="facts">
          <dt>Node</dt>
          <dd>
            {s.node.reachability.state}, last seen {formatTime(s.node.reachability.last_seen_at, now, true)}
          </dd>
          <Facts label="Process" dim={s.process} now={now} />
          <Facts label="Readiness" dim={s.readiness} now={now} />
          <dt>Desired</dt>
          <dd>{s.desired_state}</dd>
        </dl>
      </details>
    </section>
  )
}

function Facts<S extends string>({ label, dim, now }: { label: string; dim: Dimension<S>; now: Date }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>
        {dim.state}
        {dim.last_reported !== undefined && ` (last reported: ${dim.last_reported})`}, since {formatTime(dim.since, now, true)}, observed{' '}
        {formatTime(dim.observed_at, now, true)}
      </dd>
    </>
  )
}

// Setup is what a new server needs before it can start (D9): the
// install, then the Minecraft EULA, each shown only while it's missing.
// Like the buttons, a control is there only with permission, and
// disabled with its reason when the state doesn't allow it.
function Setup({ server: s, pending, onAct }: { server: Server; pending: PageAction | null; onAct: (a: PageAction) => void }) {
  const control = (action: 'install' | 'accept_eula') => {
    const a = s.actions.find((x) => x.action === action)
    if (!a) return <p className="meta">An admin has to do this.</p>
    return (
      <>
        <button type="button" className="button primary" disabled={!a.allowed || pending !== null} onClick={() => onAct(action)}>
          {pending === action ? buttonWords[action].busy : buttonWords[action].idle}
        </button>
        {!a.allowed && <p className="meta reason">{reasonText(a.reason)}</p>}
      </>
    )
  }
  if (!s.installed) {
    return (
      <section className="card setup" aria-labelledby="setup-title">
        <h2 id="setup-title" className="section-title">
          Not installed yet
        </h2>
        <p>
          Waystone downloads {gameLine(s)} from Mojang, checks it against the version it ships with, and installs it on {s.node.name}.
          Creating a server starts the install; if it didn't finish, install again.
        </p>
        {control('install')}
      </section>
    )
  }
  if (!s.eula_accepted) {
    return (
      <section className="card setup" aria-labelledby="setup-title">
        <h2 id="setup-title" className="section-title">
          Accept the Minecraft EULA
        </h2>
        <p>
          The Minecraft server only starts once you accept Mojang's{' '}
          <a href={eulaURL} target="_blank" rel="noreferrer">
            End User License Agreement
          </a>
          . Waystone records who accepted it, and when.
        </p>
        {control('accept_eula')}
      </section>
    )
  }
  return null
}

// Actions come from the server's actions list (B8): permission → not
// there at all, state → disabled with its reason underneath. Install
// and the EULA are in Setup above.
function Actions({ server: s, pending, onAct }: { server: Server; pending: PageAction | null; onAct: (a: ServerAction) => void }) {
  const actions = s.actions.flatMap((a) => (isServerAction(a.action) ? [{ ...a, action: a.action }] : []))
  if (actions.length === 0) return null
  const reasons = [...new Set(actions.filter((a) => !a.allowed).map((a) => reasonText(a.reason)))]
  return (
    <div className="actions">
      <div className="action-buttons">
        {actions.map((a) => (
          <button
            key={a.action}
            type="button"
            className={a.action === 'start' && s.installed && s.eula_accepted ? 'button primary' : 'button'}
            disabled={!a.allowed || pending !== null}
            onClick={() => onAct(a.action)}
          >
            {pending === a.action ? buttonWords[a.action].busy : buttonWords[a.action].idle}
          </button>
        ))}
      </div>
      {reasons.map((r) => (
        <p key={r} className="meta reason">
          {r}
        </p>
      ))}
    </div>
  )
}

// ActiveLine is the operation in progress, on its own line (R2).
function ActiveLine({ op, now }: { op: Operation; now: Date }) {
  const parts = [actionName(op.action), op.state]
  const t = took(op, now)
  if (op.phase) parts.push(t ? `${op.phase} (${t.replace('…', '')})` : op.phase)
  else if (t) parts.push(t)
  return (
    <Link className="active-line" to={`/operations/${encodeURIComponent(op.id)}`}>
      <StatusLine symbol="transition" tone="warn">
        {parts.join(' · ')}
      </StatusLine>
      <span className="chevron" aria-hidden="true">
        ›
      </span>
    </Link>
  )
}

function HistoryRow({ op, now }: { op: Operation; now: Date }) {
  const m = stateMark(op.state)
  return (
    <li>
      <Link className="history-row" to={`/operations/${encodeURIComponent(op.id)}`}>
        <span className="cell-action">{actionName(op.action)}</span>
        <span className="cell-state">
          <StatusLine symbol={m.symbol} tone={m.tone}>
            {stateWords[op.state]}
          </StatusLine>
        </span>
        <span className="cell-time">{formatTime(op.requested_at, now)}</span>
        <span className="cell-actor">{op.actor.name}</span>
        <span className="cell-outcome">{outcome(op, now)}</span>
        <span className="chevron" aria-hidden="true">
          ›
        </span>
      </Link>
    </li>
  )
}
