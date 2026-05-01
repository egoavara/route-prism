export interface ServiceItem {
  target: string
  // Cookie name an EdgeTransformation lifts into Baggage for this target.
  // Empty/missing when the target has no EdgeTransformation attached.
  translator?: string
  // /service responses: true when at least one alternative on this
  // target is backed by a RemoteRoute. Used by the dashboard to show a
  // "remote" badge on the service row without a per-row /alternative
  // round-trip.
  hasRemote?: boolean
  // /alternative responses: true on the synthetic row that represents
  // the default / unmarked traffic path. Explicit discriminator —
  // never compare `target === "."`.
  self?: boolean
  // /alternative responses: per-alternative RemoteRoute flags.
  // remote=true means traffic to this alt leaves the cluster for a
  // developer's PC; reachable mirrors the RR's UpstreamReachable
  // condition (true/false; undefined = status not yet reported).
  remote?: boolean
  reachable?: boolean
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
