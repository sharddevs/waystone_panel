// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useId, useState, type FormEvent } from 'react'
import { ApiError, type Enrollment, type NewEnrollment, type NodeDetail } from '../api'
import { CopyButton } from '../components/CopyButton'
import { StatusLine, type MarkSymbol } from '../components/StatusMark'
import { ago, errorText, formatTime } from '../format'
import { useLive } from '../live'
import { useSignedIn } from '../session'
import type { Tone } from '../status'
import { Problem } from './Problem'

// nodeNameLimit is the API's limit for a node's name, in bytes
// (store.MaxNodeNameBytes).
export const nodeNameLimit = 64

// shownEnrollments is how many tokens the page lists, newest first.
const shownEnrollments = 10

// The Nodes page (D9): every machine Waystone manages, adding one with a
// one-time token, and revoking one.
export function NodesPage() {
  const { client } = useSignedIn()
  const nodes = useLive(() => client.nodes())
  const enrollments = useLive(() => client.enrollments())
  const [adding, setAdding] = useState(false)
  const now = nodes.at ?? new Date()

  useEffect(() => {
    document.title = 'Nodes · Waystone'
  }, [])

  const list = nodes.data ? [...nodes.data.nodes].sort((a, b) => a.name.localeCompare(b.name)) : undefined
  const reload = async () => {
    await Promise.all([nodes.reload(), enrollments.reload()])
  }

  return (
    <>
      <div className="page-head">
        <h1 className="page-title">Nodes</h1>
        <div className="head-side">
          {list !== undefined && <p className="meta">{list.length === 1 ? '1 node' : `${list.length} nodes`}</p>}
          {!adding && list !== undefined && (
            <button type="button" className="button primary" onClick={() => setAdding(true)}>
              Add node
            </button>
          )}
        </div>
      </div>

      {adding && <AddNode now={now} onChange={() => void reload()} onClose={() => setAdding(false)} />}

      {list === undefined && nodes.error && (
        <Problem error={nodes.error} retry={() => void nodes.reload()}>
          Couldn't load nodes.
        </Problem>
      )}
      {list === undefined && !nodes.error && (
        <p className="loading" role="status">
          Loading…
        </p>
      )}

      {list !== undefined && (
        <>
          <div className="node-head" aria-hidden="true">
            <span>Status</span>
            <span>Name</span>
            <span>Agent</span>
            <span>Enrolled</span>
            <span>Servers</span>
            <span />
          </div>
          <ul className="node-list">
            {list.map((n) => (
              <NodeRow key={n.id} node={n} now={now} onRevoked={() => void reload()} />
            ))}
          </ul>
          {nodes.error && <p className="meta stale">Couldn't refresh: {errorText(nodes.error)} Showing what Waystone last reported.</p>}
        </>
      )}

      {enrollments.data !== undefined && enrollments.data.enrollments.length > 0 && (
        <section aria-labelledby="tokens-title">
          <h2 id="tokens-title" className="section-title">
            Enrollment tokens
          </h2>
          <ul className="token-list">
            {enrollments.data.enrollments.slice(0, shownEnrollments).map((e) => (
              <TokenRow key={e.id} enrollment={e} now={now} />
            ))}
          </ul>
        </section>
      )}
    </>
  )
}

function nodeStatus(n: NodeDetail, now: Date): { symbol: MarkSymbol; tone: Tone; words: string; detail?: string } {
  if (n.revoked_at !== null) return { symbol: 'stopped', tone: 'unk', words: 'Revoked', detail: formatTime(n.revoked_at, now) }
  if (n.reachability.state === 'reachable') return { symbol: 'online', tone: 'ok', words: 'Reachable' }
  const seen = ago(n.reachability.last_seen_at, now)
  return { symbol: 'unknown', tone: 'unk', words: 'Unreachable', detail: seen ? `Last seen ${seen}` : 'Never seen' }
}

function NodeRow({ node: n, now, onRevoked }: { node: NodeDetail; now: Date; onRevoked: () => void }) {
  const [confirming, setConfirming] = useState(false)
  const st = nodeStatus(n, now)
  const revoke = n.actions.find((a) => a.action === 'revoke')
  return (
    <li>
      <div className="node-row">
        <span className="cell-status">
          <StatusLine symbol={st.symbol} tone={st.tone}>
            {st.words}
          </StatusLine>
          {st.detail && <span className="why">{st.detail}</span>}
        </span>
        <span className="cell-name">
          {n.name}
          {n.link === 'local' && <span className="tag">This machine</span>}
        </span>
        <span className="cell-agent">
          <span className="phone-label">Agent </span>
          {n.agent_version ?? '—'}
        </span>
        <span className="cell-enrolled">
          {n.link === 'local' ? 'Built in' : <><span className="phone-label">Enrolled </span>{formatTime(n.enrolled_at, now)}</>}
        </span>
        <span className="cell-count">{n.server_count === 1 ? '1 server' : `${n.server_count} servers`}</span>
        <span className="cell-revoke">
          {revoke?.allowed && !confirming && (
            <button type="button" className="button small" onClick={() => setConfirming(true)} aria-label={`Revoke ${n.name}`}>
              Revoke
            </button>
          )}
        </span>
      </div>
      {confirming && <ConfirmRevoke node={n} onCancel={() => setConfirming(false)} onDone={onRevoked} />}
    </li>
  )
}

// ConfirmRevoke says plainly what revoking does and doesn't do (D5), and,
// for the controller's own node, that v0.1 has no way back (6b-I1).
function ConfirmRevoke({ node: n, onCancel, onDone }: { node: NodeDetail; onCancel: () => void; onDone: () => void }) {
  const { client, session, expired } = useSignedIn()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)
  const titleId = useId()

  async function revoke() {
    setBusy(true)
    setError(null)
    try {
      await client.revokeNode(n.id, session.csrf_token)
      onDone()
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        expired()
        return
      }
      setError(e instanceof ApiError ? e : new ApiError(0, 'network', String(e)))
      setBusy(false)
    }
  }

  return (
    <section className="confirm" role="group" aria-labelledby={titleId}>
      <p id={titleId} className="callout-title">
        Revoke {n.name}?
      </p>
      <p>Revoking stops this node from connecting. It does not stop servers already running on it.</p>
      {n.link === 'local' ? (
        <p>
          This is the machine Waystone runs on. In this version a revoked node can't be added back, so its servers can't be managed from
          Waystone again.
        </p>
      ) : (
        <p>To use the machine again later, add it as a new node. Its old servers stay with this one.</p>
      )}
      <div className="action-buttons">
        <button type="button" className="button danger" disabled={busy} onClick={() => void revoke()}>
          {busy ? 'Revoking…' : 'Revoke node'}
        </button>
        <button type="button" className="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
      <div aria-live="polite">{error && <ErrorLines error={error} />}</div>
    </section>
  )
}

// nodeNameProblem is why a name can't be used, or undefined. The API
// checks the same rule (store.ValidNodeName).
export function nodeNameProblem(name: string): string | undefined {
  if (name.trim() === '') return 'Give the node a name.'
  if (new TextEncoder().encode(name).length > nodeNameLimit) return `The name is too long: at most ${nodeNameLimit} bytes.`
  for (const ch of name) {
    const c = ch.codePointAt(0) ?? 0
    if (c < 0x20 || (c >= 0x7f && c <= 0x9f)) return 'The name can only hold plain text.'
  }
  return undefined
}

// AddNode makes a one-time token and shows it once (D2). The bundle lives
// only in this component's state: Done throws it away.
function AddNode({ now, onChange, onClose }: { now: Date; onChange: () => void; onClose: () => void }) {
  const { client, session, expired } = useSignedIn()
  const [name, setName] = useState('')
  const [tried, setTried] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)
  const [made, setMade] = useState<NewEnrollment | null>(null)
  const nameId = useId()
  const hintId = useId()
  const bundleId = useId()
  const why = nodeNameProblem(name)

  async function create(e: FormEvent) {
    e.preventDefault()
    setTried(true)
    if (why !== undefined || busy) return
    setBusy(true)
    setError(null)
    try {
      setMade(await client.createEnrollment(name, session.csrf_token))
      onChange()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        expired()
        return
      }
      setError(err instanceof ApiError ? err : new ApiError(0, 'network', String(err)))
    } finally {
      setBusy(false)
    }
  }

  if (made) {
    return (
      <section className="card add-node" aria-labelledby="add-title">
        <h2 id="add-title" className="section-title">
          Add {made.enrollment.name}
        </h2>
        <ol className="steps">
          <li>
            <p>On the new machine, with the Waystone agent installed, run:</p>
            <p>
              <code className="command">{made.command}</code> <CopyButton text={made.command} label="Copy the command" />
            </p>
          </li>
          <li>
            <label htmlFor={bundleId}>When it asks, paste this bundle:</label>
            {/* The bundle is a credential: no spellchecker (some upload what
                they check), no autofill, no autocapitalisation (6d-H1). */}
            <textarea
              id={bundleId}
              className="bundle mono"
              readOnly
              rows={3}
              value={made.bundle}
              spellCheck={false}
              autoComplete="off"
              autoCapitalize="none"
              onFocus={(e) => e.currentTarget.select()}
            />
            <CopyButton text={made.bundle} label="Copy the bundle" />
          </li>
        </ol>
        <p className="note">
          The bundle works once, until {formatTime(made.enrollment.expires_at, now)}. Waystone won't show it again: if it's lost, make a new
          one and let this one expire. Once it's pasted, copy something else so it isn't left on the clipboard.
        </p>
        <button
          type="button"
          className="button"
          onClick={() => {
            setMade(null)
            onClose()
          }}
        >
          Done
        </button>
      </section>
    )
  }

  return (
    <section className="card add-node" aria-labelledby="add-title">
      <h2 id="add-title" className="section-title">
        Add a node
      </h2>
      <p className="meta">Waystone makes a one-time token for the new machine. Name it after the machine.</p>
      <form onSubmit={(e) => void create(e)}>
        <div className="field">
          <label htmlFor={nameId}>Name</label>
          <input
            id={nameId}
            value={name}
            autoComplete="off"
            aria-describedby={hintId}
            aria-invalid={tried && why !== undefined}
            onChange={(e) => setName(e.target.value)}
          />
          <p id={hintId} className="meta">
            {tried && why ? why : `For example "homelab-2". At most ${nodeNameLimit} bytes.`}
          </p>
        </div>
        <div className="action-buttons">
          <button type="submit" className="button primary" disabled={busy}>
            {busy ? 'Making a token…' : 'Make a token'}
          </button>
          <button type="button" className="button" disabled={busy} onClick={onClose}>
            Cancel
          </button>
        </div>
      </form>
      <div aria-live="polite">{error && <ErrorLines error={error} />}</div>
    </section>
  )
}

const tokenWords: Record<Enrollment['state'], string> = { pending: 'Waiting', used: 'Used', expired: 'Expired' }

function TokenRow({ enrollment: e, now }: { enrollment: Enrollment; now: Date }) {
  const when =
    e.state === 'used'
      ? `used ${formatTime(e.used_at, now)}`
      : e.state === 'pending'
        ? `works until ${formatTime(e.expires_at, now)}`
        : `expired ${formatTime(e.expires_at, now)}`
  return (
    <li className="token-row">
      <span className="cell-name">{e.name}</span>
      <span className="cell-token-state">{tokenWords[e.state]}</span>
      <span className="meta">
        made by {e.created_by.name}, {formatTime(e.created_at, now)}; {when}
      </span>
    </li>
  )
}

function ErrorLines({ error }: { error: ApiError }) {
  return (
    <>
      <p className="problem">{errorText(error)}</p>
      {error.requestId && (
        <p className="meta">
          Request ID <span className="mono">{error.requestId}</span>
        </p>
      )}
    </>
  )
}
