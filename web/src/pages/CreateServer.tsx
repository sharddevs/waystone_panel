// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useId, useRef, useState, type FormEvent } from 'react'
import { Link, useNavigate } from 'react-router'
import { ApiError, type NewServer, type NodeDetail, type Server } from '../api'
import { errorText } from '../format'
import { useLive } from '../live'
import { useSignedIn } from '../session'
import { Problem } from './Problem'

// The pinned game version (minecraft.PinnedVersion). The API picks it
// itself; the form only shows it.
export const pinnedGame = 'Minecraft Java 26.3'

// The API's limits (store.MaxServerNameBytes, minecraft server-port,
// store.MinHeapMiB..MaxHeapMiB).
export const serverNameLimit = 100
export const minPort = 1024
export const maxPort = 65535
export const firstGamePort = 25565

// memoryChoices are the heap sizes offered, in MiB; 2 GiB is the default.
export const memoryChoices = [1024, 2048, 3072, 4096, 6144, 8192, 12288, 16384]
const defaultMemory = 2048

// nextFreePort is the first port from 25565 up that no server on node
// uses (D9), or undefined if none is left.
export function nextFreePort(servers: Server[], nodeId: string): number | undefined {
  const used = new Set(servers.filter((s) => s.node.id === nodeId && s.port !== null).map((s) => s.port))
  for (let p = firstGamePort; p <= maxPort; p++) if (!used.has(p)) return p
  return undefined
}

// serverNameProblem and portProblem say why a field can't be sent, or
// undefined. The API checks the same rules.
export function serverNameProblem(name: string): string | undefined {
  if (name.trim() === '') return 'Give the server a name.'
  if (new TextEncoder().encode(name).length > serverNameLimit) return `The name is too long: at most ${serverNameLimit} bytes.`
  for (const ch of name) {
    const c = ch.codePointAt(0) ?? 0
    if (c < 0x20 || (c >= 0x7f && c <= 0x9f)) return 'The name can only hold plain text.'
  }
  return undefined
}

export function portProblem(port: string): string | undefined {
  if (!/^\d{1,5}$/.test(port) || Number(port) < minPort || Number(port) > maxPort) return `The port must be a number from ${minPort} to ${maxPort}.`
  return undefined
}

function memoryLabel(mib: number): string {
  return `${mib / 1024} GB`
}

// usable nodes are the ones a server can be created on: not revoked.
// An unreachable node is listed but can't be picked (the API refuses it).
function usable(nodes: NodeDetail[]): NodeDetail[] {
  return nodes.filter((n) => n.revoked_at === null).sort((a, b) => a.name.localeCompare(b.name))
}

// The create form (D9): node, name, port, memory, the version shown.
// Create installs the server, then goes to its page.
export function CreateServer() {
  const { client, session, expired } = useSignedIn()
  const navigate = useNavigate()
  const nodes = useLive(() => client.nodes())
  const servers = useLive(() => client.servers())
  const [nodeId, setNodeId] = useState<string | undefined>(undefined)
  const [name, setName] = useState('')
  const [port, setPort] = useState<string | undefined>(undefined)
  const [memory, setMemory] = useState(defaultMemory)
  const [tried, setTried] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)
  // One key per request body: sending the same form again after a lost
  // answer can't create a second server.
  const key = useRef<{ body: string; key: string } | null>(null)
  const ids = { node: useId(), name: useId(), nameHint: useId(), port: useId(), portHint: useId(), memory: useId() }

  useEffect(() => {
    document.title = 'Create server · Waystone'
  }, [])

  const choices = nodes.data ? usable(nodes.data.nodes) : undefined
  // The first reachable node is picked until the person picks one.
  const node = nodeId ?? choices?.find((n) => n.reachability.state === 'reachable')?.id
  // The port follows the node until the person types one.
  const suggested = node !== undefined && servers.data ? nextFreePort(servers.data.servers, node) : undefined
  const portText = port ?? (suggested === undefined ? '' : String(suggested))
  const picked = choices?.find((n) => n.id === node)

  const nameWhy = serverNameProblem(name)
  const portWhy = portProblem(portText)
  const nodeWhy = picked === undefined ? 'Pick a reachable node.' : picked.reachability.state !== 'reachable' ? 'This node is unreachable.' : undefined

  async function create(e: FormEvent) {
    e.preventDefault()
    setTried(true)
    if (nameWhy || portWhy || nodeWhy || node === undefined || busy) return
    const body: NewServer = { node_id: node, name, port: Number(portText), heap_mib: memory }
    const text = JSON.stringify(body)
    if (key.current?.body !== text) key.current = { body: text, key: crypto.randomUUID() }
    setBusy(true)
    setError(null)
    try {
      const made = await client.createServer(body, session.csrf_token, key.current.key)
      navigate(`/servers/${encodeURIComponent(made.server.id)}`)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        expired()
        return
      }
      setError(err instanceof ApiError ? err : new ApiError(0, 'network', String(err)))
      setBusy(false)
    }
  }

  const back = (
    <p className="back">
      <Link to="/">← Servers</Link>
    </p>
  )

  if (choices === undefined) {
    return (
      <>
        {back}
        {nodes.error ? (
          <Problem error={nodes.error} retry={() => void nodes.reload()}>
            Couldn't load the nodes.
          </Problem>
        ) : (
          <p className="loading" role="status">
            Loading…
          </p>
        )}
      </>
    )
  }

  const portTaken = error?.code === 'port_taken'
  return (
    <>
      {back}
      <h1 className="page-title">Create server</h1>
      {choices.length === 0 ? (
        <p className="empty">
          There's no node to create a server on. <Link to="/nodes">Add a node</Link> first.
        </p>
      ) : (
        <form className="card create-form" onSubmit={(e) => void create(e)} noValidate>
          <div className="field">
            <label htmlFor={ids.node}>Node</label>
            <select
              id={ids.node}
              value={node ?? ''}
              aria-invalid={tried && nodeWhy !== undefined}
              onChange={(e) => {
                setNodeId(e.target.value)
                setPort(undefined)
              }}
            >
              {node === undefined && <option value="">Pick a node</option>}
              {choices.map((n) => (
                <option key={n.id} value={n.id} disabled={n.reachability.state !== 'reachable'}>
                  {n.reachability.state === 'reachable' ? n.name : `${n.name} (unreachable)`}
                </option>
              ))}
            </select>
            {tried && nodeWhy && <p className="meta problem-text">{nodeWhy}</p>}
          </div>

          <div className="field">
            <label htmlFor={ids.name}>Name</label>
            <input
              id={ids.name}
              value={name}
              autoComplete="off"
              aria-describedby={ids.nameHint}
              aria-invalid={tried && nameWhy !== undefined}
              onChange={(e) => setName(e.target.value)}
            />
            <p id={ids.nameHint} className="meta">
              {tried && nameWhy ? nameWhy : 'What your players will call it, like "Survival World".'}
            </p>
          </div>

          <div className="field">
            <label htmlFor={ids.port}>Port</label>
            <input
              id={ids.port}
              className="narrow"
              inputMode="numeric"
              value={portText}
              aria-describedby={ids.portHint}
              aria-invalid={(tried && portWhy !== undefined) || portTaken}
              onChange={(e) => setPort(e.target.value)}
            />
            <p id={ids.portHint} className="meta">
              {tried && portWhy ? portWhy : portTaken ? errorText(error) : 'The first free port on this node. Players connect to it.'}
            </p>
          </div>

          <div className="field">
            <label htmlFor={ids.memory}>Memory</label>
            <select id={ids.memory} className="narrow" value={memory} onChange={(e) => setMemory(Number(e.target.value))}>
              {memoryChoices.map((m) => (
                <option key={m} value={m}>
                  {memoryLabel(m)}
                </option>
              ))}
            </select>
          </div>

          <dl className="facts">
            <dt>Game</dt>
            <dd>{pinnedGame}</dd>
          </dl>
          <p className="meta">Waystone downloads the server from Mojang, checks it, and installs it. You accept the Minecraft EULA next.</p>

          <div className="action-buttons">
            <button type="submit" className="button primary" disabled={busy}>
              {busy ? 'Creating…' : 'Create server'}
            </button>
            <Link className="button" to="/">
              Cancel
            </Link>
          </div>
          <div aria-live="polite">
            {error && !portTaken && (
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
        </form>
      )}
    </>
  )
}
