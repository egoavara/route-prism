import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import createFuzzySearch from '@nozbe/microfuzz'
import type { TupleEntry, WidgetConfig } from './types'
import { listTuples } from './api'
import { parse, readCookie, setEntry } from './cookieStore'
import { useHotkey } from './useHotkey'
import { TupleList, type TupleRow } from './components/TupleList'

interface Props {
  config: WidgetConfig
}

export function App({ config }: Props) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [items, setItems] = useState<TupleEntry[]>([])
  const [cursor, setCursor] = useState(0)
  const [entries, setEntries] = useState<Map<string, string>>(() => readEntries(config))

  const inputRef = useRef<HTMLInputElement | null>(null)
  const rootRef = useRef<HTMLDivElement | null>(null)
  const reqRef = useRef(0)
  // Snapshot of own-tier variant at mount. Reload is required only when
  // *this* tier's variant has drifted from what the page was rendered
  // against; cross-tier overrides take effect on the next downstream call
  // and don't need a host-page reload.
  const initialOwnVariantRef = useRef<string>(entries.get(config.routingKey) ?? '')

  const mode = config.style?.mode ?? 'float'
  const anchor = config.style?.anchor ?? (mode === 'float' ? 'bottom-right' : 'right')
  const margin = config.style?.margin ?? {}
  const hostStyle: Record<string, string> = {}
  if (margin.top) hostStyle['--rp-margin-top'] = margin.top
  if (margin.right) hostStyle['--rp-margin-right'] = margin.right
  if (margin.bottom) hostStyle['--rp-margin-bottom'] = margin.bottom
  if (margin.left) hostStyle['--rp-margin-left'] = margin.left

  const ownVariant = entries.get(config.routingKey) ?? ''
  const isOwnActive = ownVariant !== '' && ownVariant !== '.'
  const hasAnyActive = useMemo(() => {
    for (const v of entries.values()) if (v && v !== '.') return true
    return false
  }, [entries])
  const needsReload = ownVariant !== initialOwnVariantRef.current

  // Open / close.
  const handleOpen = useCallback(() => setOpen((v) => !v), [])
  useHotkey(config.style?.hotkey?.open, handleOpen)

  useEffect(() => {
    if (!open) return
    inputRef.current?.focus()
    inputRef.current?.select()
  }, [open])

  // Click-outside-to-close. We listen on the host document; composedPath()
  // crosses the Shadow DOM boundary so we can tell whether the original
  // click target lives inside our root.
  useEffect(() => {
    if (!open) return
    const handler = (e: MouseEvent) => {
      const root = rootRef.current
      if (!root) return
      if (e.composedPath().includes(root)) return
      setOpen(false)
    }
    document.addEventListener('mousedown', handler, true)
    return () => document.removeEventListener('mousedown', handler, true)
  }, [open])

  // Fetch tuples (debounced via reqRef counter to avoid out-of-order updates).
  useEffect(() => {
    if (!open) return
    const myReq = ++reqRef.current
    const handle = window.setTimeout(() => {
      listTuples(config, query)
        .then((resp) => {
          if (myReq !== reqRef.current) return
          setItems(resp.items ?? [])
          setCursor(0)
        })
        .catch(() => {
          if (myReq !== reqRef.current) return
          setItems([])
        })
    }, 120)
    return () => window.clearTimeout(handle)
  }, [open, query, config])

  // Build fuzzy index for client-side scoring + highlighting on top of
  // the server's substring filter.
  const rows: TupleRow[] = useMemo(() => {
    if (items.length === 0) return []
    if (!query) {
      return items.map((entry) => ({ entry, matches: [] as number[] }))
    }
    const fuzzy = createFuzzySearch(items, {
      getText: (i: TupleEntry) => [i.tuple],
    })
    const scored = fuzzy(query)
    return scored.map((res) => {
      const matches: number[] = []
      for (const range of res.matches[0] ?? []) {
        for (let i = range[0]; i <= range[1]; i++) matches.push(i)
      }
      return { entry: res.item, matches }
    })
  }, [items, query])

  const commit = useCallback(
    (entry: TupleEntry) => {
      const cookieName = entry.sourceCookie || config.sourceCookie
      if (!cookieName) return
      setEntry(cookieName, entry.routingKey, entry.alternative)
      setEntries(parse(readCookie(cookieName)))
    },
    [config],
  )

  // Keyboard navigation inside the panel.
  const onSearchKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false)
        return
      }
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        setCursor((c) => Math.min(c + 1, Math.max(rows.length - 1, 0)))
        return
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault()
        setCursor((c) => Math.max(c - 1, 0))
        return
      }
      if (e.key === 'Enter') {
        e.preventDefault()
        const row = rows[cursor]
        if (row) commit(row.entry)
        return
      }
    },
    [rows, cursor, commit],
  )

  const reload = useCallback(() => window.location.reload(), [])

  const buttonLabel = isOwnActive ? `route: ${ownVariant}` : 'route: default'
  // Yellow takes precedence over blue: if this tier needs a reload the
  // user should see the reload affordance regardless of any other active
  // overrides.
  const stateClass = needsReload ? 'rp-needs-reload' : hasAnyActive ? 'rp-active' : ''

  return (
    <div
      ref={rootRef}
      class={`rp-root rp-mode-${mode} rp-anchor-${anchor}`}
      style={hostStyle}
    >
      {open && (
        <div class="rp-panel">
          <div class="rp-header">
            <input
              ref={inputRef}
              class="rp-search"
              type="text"
              placeholder="fuzzy search…"
              value={query}
              onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
              onKeyDown={onSearchKeyDown}
              spellcheck={false}
              autocomplete="off"
            />
          </div>
          <TupleList
            rows={rows}
            cursor={cursor}
            entries={entries}
            onHover={setCursor}
            onSelect={commit}
          />
          {needsReload && (
            <div class="rp-banner">
              <span>own-tier variant changed — reload to apply</span>
              <button onClick={reload}>reload</button>
            </div>
          )}
        </div>
      )}
      {mode === 'sidebar' ? (
        <button
          class={`rp-tab ${stateClass}`}
          onClick={handleOpen}
          title={buttonLabel}
        >
          route prism
        </button>
      ) : (
        <button
          class={`rp-button ${stateClass}`}
          onClick={handleOpen}
          title={buttonLabel}
        >
          <span class="rp-dot" />
          {buttonLabel}
        </button>
      )}
    </div>
  )
}

function readEntries(cfg: WidgetConfig): Map<string, string> {
  if (!cfg.sourceCookie) return new Map()
  return parse(readCookie(cfg.sourceCookie))
}
