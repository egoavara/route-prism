export interface ServiceItem {
  target: string
  // Cookie name an EdgeTransformation lifts into Baggage for this target.
  // Empty/missing when the target has no EdgeTransformation attached.
  translator?: string
}

export interface ListResponse {
  items: ServiceItem[]
  nextCursor?: string
  // Populated only by /alternative responses.
  routingKey?: string
  sourceCookie?: string
}

export interface ListParams {
  fuzzy?: string
  startswith?: string
  equals?: string
  limit?: number
  cursor?: string
}

const buildQuery = (params: ListParams): string => {
  const q = new URLSearchParams()
  if (params.fuzzy) q.set('target.fuzzy', params.fuzzy)
  if (params.startswith) q.set('target.startswith', params.startswith)
  if (params.equals) q.set('target.equals', params.equals)
  if (params.limit) q.set('limit', String(params.limit))
  if (params.cursor) q.set('cursor', params.cursor)
  const s = q.toString()
  return s ? `?${s}` : ''
}

const apiBase = '/api/v1'

export async function listServices(params: ListParams = {}): Promise<ListResponse> {
  const res = await fetch(`${apiBase}/service${buildQuery(params)}`)
  if (!res.ok) throw new Error(`listServices failed: ${res.status}`)
  return res.json()
}

export async function listAlternatives(
  target: string,
  params: ListParams = {},
): Promise<ListResponse> {
  const res = await fetch(
    `${apiBase}/service/${encodeURIComponent(target)}/alternative${buildQuery(params)}`,
  )
  if (!res.ok) throw new Error(`listAlternatives failed: ${res.status}`)
  return res.json()
}
