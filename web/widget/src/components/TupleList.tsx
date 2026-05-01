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
    const cls = ['rp-alt', isCursor && 'rp-cursor', isCurrent && 'rp-current']
      .filter(Boolean)
      .join(' ')
    // tuple = "<service>:<alternative>"; matches index into the tuple, so
    // the alternative portion starts right after the colon.
    const altOffset = row.entry.tuple.indexOf(':') + 1
    out.push(
      <li
        key={row.entry.tuple}
        class={cls}
        onMouseEnter={() => onHover(idx)}
        onClick={() => onSelect(row.entry)}
      >
        <span class={`rp-radio ${isCurrent ? 'rp-radio-on' : ''}`} aria-hidden="true" />
        <span class="rp-alt-name">
          {renderHighlighted(row.entry.alternative, row.matches, altOffset)}
        </span>
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
