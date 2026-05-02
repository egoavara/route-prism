import type { TupleListResponse, WidgetConfig } from './types'

export function apiBase(cfg: WidgetConfig): string {
  if (cfg.previewMode) return '/api/v1'
  // Translator proxies /<pathPrefix>/api/ → operator /api/.
  // Same-origin → no CORS dance.
  return `/${cfg.pathPrefix}/api/v1`
}

export async function listTuples(
  cfg: WidgetConfig,
  fuzzy: string,
  limit = 50,
): Promise<TupleListResponse> {
  const q = new URLSearchParams()
  if (fuzzy) q.set('fuzzy', fuzzy)
  q.set('limit', String(limit))
  const url = `${apiBase(cfg)}/tuple?${q}`
  const res = await fetch(url, { credentials: 'omit' })
  if (!res.ok) throw new Error(`listTuples failed: ${res.status}`)
  return res.json()
}
