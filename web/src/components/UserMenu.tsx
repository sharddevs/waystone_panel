// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useId, useRef, useState } from 'react'
import { densities, modes, themes, type Preferences } from '../appearance'
import { useSession } from '../session'

const labels: Record<string, string> = {
  stone: 'Stone',
  slate: 'Slate',
  comfortable: 'Comfortable',
  compact: 'Compact',
  dark: 'Dark',
  light: 'Light',
  system: 'System',
}

function Choice<K extends keyof Preferences>({
  name,
  legend,
  values,
  prefs,
  onPick,
}: {
  name: K
  legend: string
  values: readonly Preferences[K][]
  prefs: Preferences
  onPick: (p: Preferences) => void
}) {
  const id = useId()
  return (
    <fieldset className="choice">
      <legend>{legend}</legend>
      {values.map((v) => (
        <label key={v} className="choice-option">
          <input type="radio" name={`${id}-${name}`} value={v} checked={prefs[name] === v} onChange={() => onPick({ ...prefs, [name]: v })} />
          {labels[v]}
        </label>
      ))}
    </fieldset>
  )
}

// UserMenu is "trevor ▾" in the top bar: Appearance (Q10) above Sign out.
// It is a disclosure, not an ARIA menu: a button that opens a panel of
// ordinary form controls, all reachable with Tab; Escape closes it.
export function UserMenu() {
  const session = useSession()
  const [open, setOpen] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const button = useRef<HTMLButtonElement>(null)
  const panel = useRef<HTMLDivElement>(null)
  const panelId = useId()

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false)
        button.current?.focus()
      }
    }
    const onClick = (e: MouseEvent) => {
      const t = e.target as Node
      if (!panel.current?.contains(t) && !button.current?.contains(t)) setOpen(false)
    }
    document.addEventListener('keydown', onKey)
    document.addEventListener('mousedown', onClick)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('mousedown', onClick)
    }
  }, [open])

  if (session.state.status !== 'signed-in') return null
  const { session: s } = session.state

  const pick = (p: Preferences) => {
    setError(null)
    session.setPreferences(p).catch(() => setError("Couldn't save the appearance settings. They're back to what they were."))
  }
  const signOut = () => {
    setError(null)
    session.signOut().catch(() => setError("Couldn't sign out. Try again."))
  }

  return (
    <div className="user-menu">
      <button ref={button} type="button" className="user-button" aria-expanded={open} aria-controls={panelId} onClick={() => setOpen((o) => !o)}>
        {s.user.display_name} <span aria-hidden="true">▾</span>
      </button>
      <div ref={panel} id={panelId} className="user-panel" hidden={!open}>
        <section aria-label="Appearance">
          <h2 className="panel-heading">Appearance</h2>
          <Choice name="theme" legend="Theme" values={themes} prefs={s.preferences} onPick={pick} />
          <Choice name="density" legend="Density" values={densities} prefs={s.preferences} onPick={pick} />
          <Choice name="mode" legend="Mode" values={modes} prefs={s.preferences} onPick={pick} />
        </section>
        <div aria-live="polite">{error && <p className="problem">{error}</p>}</div>
        <button type="button" className="button wide" onClick={signOut}>
          Sign out
        </button>
      </div>
    </div>
  )
}
