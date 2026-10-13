import type { ApiClient } from '@shared/http/client'
import { integer, list, record, text } from './response'

/** Casdoor 团队及平台管理员授权给它的分组。 */
export interface TeamItem {
  name: string
  displayName: string
  groupIDs: number[]
}
export interface TeamGroup {
  id: number
  name: string
}
export interface TeamMember {
  id: string
  name: string
  displayName: string
}
/** 团队管理员为成员创建密钥所需的信息。 */
export interface TeamDetail {
  groups: TeamGroup[]
  members: TeamMember[]
}

export const teamsKey = ['modern', 'teams'] as const
export const teamDetailKey = (team: string) => ['modern', 'team-detail', team] as const

const teamPath = (team: string) => `/api/teams/${encodeURIComponent(team)}` as const

export async function getTeams(client: ApiClient, signal: AbortSignal): Promise<TeamItem[]> {
  const data = record(await client.request('/api/teams', { signal }))
  return list(data.items).map((value) => {
    const row = record(value)
    return {
      name: text(row.name),
      displayName: text(row.display_name),
      groupIDs: list(row.group_ids).map((id) => integer(id, 1)),
    }
  })
}

export async function getTeam(
  client: ApiClient,
  team: string,
  signal: AbortSignal,
): Promise<TeamDetail> {
  const data = record(await client.request(teamPath(team), { signal }))
  return {
    groups: list(data.groups).map((value) => {
      const row = record(value)
      return { id: integer(row.id, 1), name: text(row.name) }
    }),
    members: list(data.members).map((value) => {
      const row = record(value)
      return { id: text(row.id), name: text(row.name), displayName: text(row.display_name) }
    }),
  }
}

export async function updateTeamGroups(
  client: ApiClient,
  team: string,
  groupIDs: number[],
  signal: AbortSignal,
): Promise<void> {
  await client.request(`${teamPath(team)}/groups`, {
    method: 'PUT',
    json: { group_ids: groupIDs },
    signal,
  })
}
