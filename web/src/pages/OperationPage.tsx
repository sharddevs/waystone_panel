// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useId, useState, type FormEvent } from 'react'
import { Link, useParams } from 'react-router'
import { ApiError, type Operation } from '../api'
import { CopyButton } from '../components/CopyButton'
import { StatusLine } from '../components/StatusMark'
import { actionName, errorText, formatTime, isActive, stateMark, stateWords, took, uncertainText } from '../format'
import { useLive } from '../live'
import { useSignedIn } from '../session'
import { Problem } from './Problem'

// noteLimit is the API's limit for a resolution note, in bytes.
export const noteLimit = 500

export function OperationPageRoute() {
  const { id = '' } = useParams()
  return <OperationPage key={id} id={id} />
}

// Operation details (wireframes §4, W2), with the resolve form for an
// uncertain operation (W3).
export function OperationPage({ id }: { id: string }) {
  const { client } = useSignedIn()
  const op = useLive(async () => {
    const o = await client.operation(id)
    // The server's name for the back link and the "Server" line. The
    // operation outlives its server, so a missing server is no error.
    let serverName: string | undefined
    try {
      serverName = (await client.server(o.server_id)).name
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) throw e
    }
    return { o, serverName }
  })

  const title = op.data ? actionName(op.data.o.action) : 'Operation'
  useEffect(() => {
    document.title = `${title} · Waystone`
  }, [title])

  if (!op.data) {
    return (
      <>
        <p className="back">
          <Link to="/">← Servers</Link>
        </p>
        {op.error ? (
          <Problem error={op.error} retry={() => void op.reload()}>
            {op.error.code === 'not_found' ? "Couldn't find this operation." : "Couldn't load this operation."}
          </Problem>
        ) : (
          <p className="loading" role="status">
            Loading…
          </p>
        )}
      </>
    )
  }

  const { o, serverName } = op.data
  const now = op.at ?? new Date()
  const m = stateMark(o.state)
  const serverLink = `/servers/${encodeURIComponent(o.server_id)}`

  return (
    <>
      <p className="back">{serverName ? <Link to={serverLink}>← {serverName}</Link> : <Link to="/">← Servers</Link>}</p>

      <h1 className="page-title op-title">
        {actionName(o.action)}
        <span className="op-state">
          <StatusLine symbol={m.symbol} tone={m.tone}>
            {stateWords[o.state]}
          </StatusLine>
        </span>
      </h1>
      <div aria-live="polite">
        {isActive(o) && (
          <p className="detail">
            {o.state === 'pending' ? 'Waiting to run.' : o.phase ? `Now ${o.phase}.` : 'In progress.'} {took(o, now) && `Elapsed ${took(o, now)?.replace('…', '')}.`}
          </p>
        )}
        {o.state === 'failed' && o.phase && <p className="detail">Failed while: {o.phase}</p>}
        {o.state === 'uncertain' && <p className="detail">{uncertainText(o.action)}</p>}
      </div>
      {op.error && <p className="meta stale">Couldn't refresh: {errorText(op.error)}</p>}

      <dl className="facts timeline">
        <dt>Requested</dt>
        <dd>
          {formatTime(o.requested_at, now, true)} by {o.actor.name}
        </dd>
        <dt>Started</dt>
        <dd>{o.started_at ? formatTime(o.started_at, now, true) : 'Not yet'}</dd>
        {!isActive(o) && (
          <>
            <dt>Finished</dt>
            <dd>
              {o.finished_at ? formatTime(o.finished_at, now, true) : o.state === 'uncertain' ? 'Not known' : '—'}
              {o.finished_at && took(o, now) && ` (${took(o, now)})`}
            </dd>
          </>
        )}
      </dl>

      {o.state === 'failed' && (o.error_summary || o.error_code) && (
        <section aria-labelledby="what-title">
          <h2 id="what-title" className="section-title">
            What happened
          </h2>
          <p>
            {o.error_summary ?? 'The operation failed.'} {o.error_code && <span className="mono meta">({o.error_code})</span>}
          </p>
        </section>
      )}

      {o.state === 'uncertain' && o.resolution === null && <ResolveForm op={o} onDone={() => void op.reload()} />}
      {o.resolution && (
        <section aria-labelledby="resolved-title">
          <h2 id="resolved-title" className="section-title">
            Resolved
          </h2>
          <p>
            Marked as {o.resolution.outcome} by {o.resolution.by.name} at {formatTime(o.resolution.at, now)}
            {o.resolution.note && <>: “{o.resolution.note}”</>}
          </p>
        </section>
      )}

      <dl className="facts ids">
        <dt>Server</dt>
        <dd>{serverName ? <Link to={serverLink}>{serverName}</Link> : <span className="mono">{o.server_id}</span>}</dd>
        <dt>Operation</dt>
        <dd>
          <span className="mono">{o.id}</span> <CopyButton text={o.id} />
        </dd>
      </dl>
    </>
  )
}

// noteProblem is why a note can't be sent, or undefined. The API checks
// the same rules (store.validNote); this only says it sooner.
export function noteProblem(note: string): string | undefined {
  if (note.trim() === '') return 'Write a note: what you checked, and what you found.'
  if (new TextEncoder().encode(note).length > noteLimit) return `The note is too long: at most ${noteLimit} bytes.`
  for (const ch of note) {
    const c = ch.codePointAt(0) ?? 0
    const control = (c < 0x20 && c !== 0x0a) || (c >= 0x7f && c <= 0x9f)
    const bidi = c === 0x061c || c === 0x200e || c === 0x200f || (c >= 0x202a && c <= 0x202e) || (c >= 0x2066 && c <= 0x2069)
    if (control || bidi) return 'The note can only hold plain text and line breaks.'
  }
  return undefined
}

// ResolveForm lets an admin say what really happened (W3). The state stays
// "uncertain"; the resolution is shown under it (C2, C7). In v0.1 every
// account is an admin; anyone else gets the API's refusal, shown here.
function ResolveForm({ op, onDone }: { op: Operation; onDone: () => void }) {
  const { client, session, expired } = useSignedIn()
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState<'succeeded' | 'failed' | null>(null)
  const [tried, setTried] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)
  const noteId = useId()
  const hintId = useId()
  const why = noteProblem(note)

  async function resolve(outcome: 'succeeded' | 'failed') {
    setTried(true)
    if (why !== undefined || busy) return
    setBusy(outcome)
    setError(null)
    try {
      await client.resolve(op.id, outcome, note, session.csrf_token, crypto.randomUUID())
      onDone()
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        expired()
        return
      }
      setError(e instanceof ApiError ? e : new ApiError(0, 'network', String(e)))
      if (e instanceof ApiError && (e.code === 'already_resolved' || e.code === 'not_uncertain')) onDone()
    } finally {
      setBusy(null)
    }
  }

  return (
    <section className="card resolve" aria-labelledby="resolve-title">
      <h2 id="resolve-title" className="section-title">
        Resolve this operation
      </h2>
      <p className="meta">Actions on this server stay unavailable until it is resolved. Check the server first, then say what happened.</p>
      <form onSubmit={(e: FormEvent) => e.preventDefault()}>
        <div className="field">
          <label htmlFor={noteId}>Note (required)</label>
          <textarea
            id={noteId}
            rows={3}
            value={note}
            aria-describedby={hintId}
            aria-invalid={tried && why !== undefined}
            onChange={(e) => setNote(e.target.value)}
          />
          <p id={hintId} className="meta">
            {tried && why ? why : `What you checked, and what you found. At most ${noteLimit} bytes.`}
          </p>
        </div>
        <div className="action-buttons">
          <button type="button" className="button" disabled={busy !== null} onClick={() => void resolve('succeeded')}>
            {busy === 'succeeded' ? 'Saving…' : 'Mark as succeeded'}
          </button>
          <button type="button" className="button" disabled={busy !== null} onClick={() => void resolve('failed')}>
            {busy === 'failed' ? 'Saving…' : 'Mark as failed'}
          </button>
        </div>
      </form>
      <div aria-live="polite">
        {error && (
          <>
            <p className="problem">{errorText(error)}</p>
            {error.requestId && (
              <p className="meta">
                Request ID <span className="mono">{error.requestId}</span>
              </p>
            )}
          </>
        )}
      </div>
    </section>
  )
}
