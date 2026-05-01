import { useEffect } from 'preact/hooks'

export interface ParsedChord {
  ctrl: boolean
  alt: boolean
  shift: boolean
  meta: boolean
  key: string
}

// formatChord renders a chord spec ("ctrl+\\") in the conventional
// platform style ("Ctrl+\\" or "⌘\\" on mac). Returns "" for unparseable
// or empty specs so the caller can omit the hint without branching.
export function formatChord(spec: string | undefined): string {
  const c = spec ? parseChord(spec) : null
  if (!c) return ''
  const isMac =
    typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform)
  const parts: string[] = []
  if (c.ctrl) parts.push(isMac ? '⌃' : 'Ctrl')
  if (c.alt) parts.push(isMac ? '⌥' : 'Alt')
  if (c.shift) parts.push(isMac ? '⇧' : 'Shift')
  if (c.meta) parts.push(isMac ? '⌘' : 'Meta')
  // Pretty-print common keys; otherwise capitalize the first character.
  let key = c.key
  if (key === 'esc') key = 'Esc'
  else if (key === 'space') key = 'Space'
  else if (key.length === 1) key = key.toUpperCase()
  else key = key.charAt(0).toUpperCase() + key.slice(1)
  parts.push(key)
  return isMac ? parts.join('') : parts.join('+')
}

export function parseChord(spec: string): ParsedChord | null {
  if (!spec) return null
  const parts = spec
    .toLowerCase()
    .split('+')
    .map((s) => s.trim())
    .filter(Boolean)
  if (parts.length === 0) return null
  const out: ParsedChord = { ctrl: false, alt: false, shift: false, meta: false, key: '' }
  for (const p of parts) {
    if (p === 'ctrl' || p === 'control') out.ctrl = true
    else if (p === 'alt' || p === 'option') out.alt = true
    else if (p === 'shift') out.shift = true
    else if (p === 'meta' || p === 'cmd' || p === 'super') out.meta = true
    else out.key = p
  }
  if (!out.key) return null
  return out
}

export function useHotkey(spec: string | undefined, onTrigger: () => void): void {
  useEffect(() => {
    const chord = spec ? parseChord(spec) : null
    if (!chord) return
    const handler = (e: KeyboardEvent) => {
      if (e.ctrlKey !== chord.ctrl) return
      if (e.altKey !== chord.alt) return
      if (e.shiftKey !== chord.shift) return
      if (e.metaKey !== chord.meta) return
      // Compare against e.key (case-insensitive). Special-case some
      // friendly aliases like "esc" / "space".
      const k = e.key.toLowerCase()
      const want = chord.key === 'esc' ? 'escape' : chord.key === 'space' ? ' ' : chord.key
      if (k !== want) return
      e.preventDefault()
      e.stopPropagation()
      onTrigger()
    }
    window.addEventListener('keydown', handler, true)
    return () => window.removeEventListener('keydown', handler, true)
  }, [spec, onTrigger])
}
