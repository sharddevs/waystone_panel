// SPDX-License-Identifier: AGPL-3.0-only

import { useId, useState, type FormEvent } from 'react'
import { ApiError, type Operation, type Server } from '../api'
import { errorText } from '../format'
import { useSignedIn } from '../session'
import { reasonText } from '../status'

// The console command box under the log (7b, scope D2): admins send one
// line to the running server. The API, the node and the database check
// the line again; this check only says what's wrong before sending.

// maxCommandBytes is console.MaxLineBytes.
export const maxCommandBytes = 256

// Control characters (tab and newline among them) and the bidi controls,
// as console.ValidLine.
// eslint-disable-next-line no-control-regex
const refusedChars = /[\u0000-\u001f\u007f-\u009f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/

// commandProblem is why line can't be sent, or undefined.
export function commandProblem(line: string): string | undefined {
  if (line === '') return 'Type a command.'
  if (line.startsWith('/')) return 'Leave out the leading /: the console takes commands without it.'
  if (refusedChars.test(line)) return 'A command is one line, with no control characters.'
  const bytes = new TextEncoder().encode(line).length
  if (bytes > maxCommandBytes) return `A command is at most ${maxCommandBytes} bytes; this one is ${bytes}.`
  return undefined
}

export function CommandBox({ server: s, onSent }: { server: Server; onSent: (op: Operation) => void }) {
  const { client, session, expired } = useSignedIn()
  const ids = { input: useId(), hint: useId() }
  const [line, setLine] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)
  const action = s.actions.find((a) => a.action === 'command')
  // Not an admin: no box at all, as with the buttons (B8).
  if (!action) return null

  const problem = commandProblem(line)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (problem !== undefined || !action.allowed || busy) return
    setBusy(true)
    setError(null)
    try {
      // One key per send: a retried request can't run twice.
      const op = await client.command(s.id, line, session.csrf_token, crypto.randomUUID())
      setLine('')
      onSent(op)
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

  return (
    <form className="command-box" onSubmit={(e) => void submit(e)}>
      <div className="command-row">
        <input
          id={ids.input}
          className="mono"
          aria-label="Console command"
          value={line}
          placeholder="say Back in five minutes"
          autoComplete="off"
          autoCapitalize="off"
          spellCheck={false}
          disabled={!action.allowed}
          aria-describedby={ids.hint}
          aria-invalid={line !== '' && problem !== undefined}
          onChange={(e) => setLine(e.target.value)}
        />
        <button type="submit" className="button" disabled={!action.allowed || busy || problem !== undefined}>
          {busy ? 'Sending…' : 'Send'}
        </button>
      </div>
      <div aria-live="polite">
        {!action.allowed && <p className="meta reason">{reasonText(action.reason)}</p>}
        {action.allowed && line !== '' && problem !== undefined && <p className="problem-text">{problem}</p>}
        {error && (
          <div className="problem-block">
            <p className="problem">{errorText(error)}</p>
            {error.requestId && (
              <p className="meta">
                Request ID <span className="mono">{error.requestId}</span>
              </p>
            )}
          </div>
        )}
      </div>
      <p id={ids.hint} className="meta">
        Every command is kept in the operation history with who sent it, and every admin can read it. Don't type passwords.
      </p>
    </form>
  )
}
