import { Fragment } from 'preact'
import type { TupleEntry } from '../types'

export interface TupleRow {
  entry: TupleEntry
  // Indices of characters in `entry.tuple` that matched the search.
  // Used for highlight rendering. Empty when there's no active search.
  matches: number[]
}

interface Props {
  rows: TupleRow[]
  cursor: number
  // Active cookie entries (routingKey → variant). A row is "current" iff
  // its (routingKey, alternative) matches an entry in this map. Multiple
  // services can be current at once — one per routingKey.
  entries: Map<string, string>
  onSelect: (entry: TupleEntry) => void
  onHover: (idx: number) => void
}

// Rows arrive sorted by `<service>:<alternative>` so same-service rows are
// already contiguous. Render them as a service header followed by the
// service's alternatives so the radio-group semantics ("exactly one
// alternative per service is active") read at a glance. The cursor still
// indexes into the flat `rows` array — headers are non-interactive.
export function TupleList({ rows, cursor, entries, onSelect, onHover }: Props) {
  if (rows.length === 0) {
    return <div class="rp-empty">no matches</div>
  }

  const out: preact.JSX.Element[] = []
  let prevKey = ''
  rows.forEach((row, idx) => {
    if (row.entry.routingKey !== prevKey) {
      // Header uses the first-seen row's matches to highlight the service
      // portion (matches in the service prefix are identical across all
      // rows of the same service).
      out.push(
        <li key={`hdr:${row.entry.routingKey}`} class="rp-group">
          <span class="rp-group-svc">{renderHighlighted(row.entry.service, row.matches, 0)}</span>
        </li>,
      )
      prevKey = row.entry.routingKey
    }
    const isCursor = idx === cursor
    const isCurrent = entries.get(row.entry.routingKey) === row.entry.alternative
    // Remote tuples (RemoteRoute-backed) carry a tristate `reachable`:
    //   true      → 'rp-remote-online'  green dot
    //   false     → 'rp-remote-offline' red dot, struck-through label
    //   undefined → 'rp-remote-unknown' grey dot
    const remoteState = row.entry.remote
      ? row.entry.reachable === true
        ? 'rp-remote-online'
        : row.entry.reachable === false
          ? 'rp-remote-offline'
          : 'rp-remote-unknown'
      : ''
    const cls = ['rp-alt', isCursor && 'rp-cursor', isCurrent && 'rp-current', remoteState]
      .filter(Boolean)
      .join(' ')
    const titleParts: string[] = []
    if (row.entry.remote) {
      titleParts.push('remote (RemoteRoute)')
      if (row.entry.reachable === false)
        titleParts.push('host PC offline — selecting will return 5xx')
      else if (row.entry.reachable === true) titleParts.push('host PC reachable')
      else titleParts.push('reachability unknown')
    }
    // tuple = "<service>:<alternative>"; matches index into the tuple, so
    // the alternative portion starts right after the colon.
    const altOffset = row.entry.tuple.indexOf(':') + 1
    out.push(
      <li
        key={row.entry.tuple}
        class={cls}
        title={titleParts.join(' — ') || undefined}
        onMouseEnter={() => onHover(idx)}
        onClick={() => onSelect(row.entry)}
      >
        <span class={`rp-radio ${isCurrent ? 'rp-radio-on' : ''}`} aria-hidden="true" />
        <span class="rp-alt-name">
          {renderHighlighted(row.entry.alternative, row.matches, altOffset)}
        </span>
        {row.entry.remote && (
          <span class="rp-remote-badge" aria-label="remote variant">
            <span class="rp-remote-dot" aria-hidden="true" />
            {row.entry.reachable === false ? 'offline' : 'remote'}
          </span>
        )}
      </li>,
    )
  })

  return <ul class="rp-list">{out}</ul>
}

// Highlight characters of `display` (a slice of the original tuple) at
// the indices provided in `matches`, which index into the WHOLE tuple
// string. `offset` says where in the tuple `display` starts.
function renderHighlighted(display: string, matches: number[], offset: number) {
  if (matches.length === 0) return <Fragment>{display}</Fragment>
  const set = new Set(matches.map((i) => i - offset).filter((i) => i >= 0 && i < display.length))
  const out: any[] = []
  let buf = ''
  for (let i = 0; i < display.length; i++) {
    if (set.has(i)) {
      if (buf) {
        out.push(buf)
        buf = ''
      }
      out.push(<mark key={i}>{display[i]}</mark>)
    } else {
      buf += display[i]
    }
  }
  if (buf) out.push(buf)
  return <Fragment>{out}</Fragment>
}
