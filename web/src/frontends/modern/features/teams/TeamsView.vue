<script setup lang="ts">
import { Users } from '@lucide/vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { computed, onScopeDispose, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { getGroupWorkspace, groupQueryKey } from '@modern/api/groups'
import {
  getTeams,
  teamDetailKey,
  teamsKey,
  updateTeamGroups,
  type TeamItem,
} from '@modern/api/teams'
import { useMessages, useMessageSource } from '@modern/app/messages'
import { usePageRefresh } from '@modern/app/page-refresh'
import { AppButton, AppCollectionState, AppMultiSelect, AppPanel } from '@modern/components/ui'
import { useLoadingActivity } from '@modern/components/ui/loading'
import { useApiClient } from '@shared/http/client-context'

const { t } = useI18n()
const client = useApiClient()
const cache = useQueryClient()
const messages = useMessages()
const controller = new AbortController()
const teams = useQuery({
  queryKey: teamsKey,
  queryFn: ({ signal }) => getTeams(client, signal),
})
const groups = useQuery({
  queryKey: groupQueryKey,
  queryFn: ({ signal }) => getGroupWorkspace(client, signal),
})
// 只保存用户改过的团队，键为团队名；没改过的团队直接显示服务端值，刷新不会冲掉编辑。
const edits = ref<Record<string, string[]>>({})
const saving = ref(new Set<string>())
const serverGroups = (team: TeamItem) => team.groupIDs.map(String)
const selected = (team: TeamItem) => edits.value[team.name] ?? serverGroups(team)
function select(team: TeamItem, values: string[]): void {
  edits.value[team.name] = values
}
const groupOptions = computed(() =>
  (groups.data.value?.items ?? []).map((group) => ({
    value: String(group.id),
    label: group.name,
    keywords: [group.channelName, group.channelID],
  })),
)
const dirty = (team: TeamItem) =>
  [...selected(team)].sort().join() !== [...serverGroups(team)].sort().join()

async function save(team: TeamItem): Promise<void> {
  if (saving.value.has(team.name)) return
  saving.value.add(team.name)
  try {
    await updateTeamGroups(client, team.name, selected(team).map(Number), controller.signal)
    if (controller.signal.aborted) return
    delete edits.value[team.name]
    await cache.invalidateQueries({ queryKey: teamsKey })
    await cache.invalidateQueries({ queryKey: teamDetailKey(team.name) })
    messages.show({
      text: t('teams.saved', { name: team.displayName || team.name }),
      tone: 'success',
    })
  } catch {
    if (!controller.signal.aborted) messages.show({ text: t('teams.failed'), tone: 'danger' })
  } finally {
    saving.value.delete(team.name)
  }
}

useLoadingActivity(() => saving.value.size > 0)
usePageRefresh({
  refresh: async () => {
    await Promise.all([teams.refetch(), groups.refetch()])
  },
  pending: () => teams.isFetching.value || saving.value.size > 0,
  updatedAt: () => teams.dataUpdatedAt.value,
})
useMessageSource(() =>
  groups.isError.value
    ? {
        text: t('teams.groupsFailed'),
        tone: 'warning',
        action: { label: t('ui.retry'), run: () => groups.refetch() },
      }
    : undefined,
)
onScopeDispose(() => controller.abort())
</script>

<template>
  <div class="modern-teams-workspace">
    <p class="modern-teams-help">{{ t('teams.help') }}</p>
    <AppCollectionState v-if="teams.isPending.value" :title="t('collection.loading')" loading />
    <AppCollectionState v-else-if="teams.isError.value" :title="t('teams.loadFailed')" error
      ><AppButton @click="teams.refetch()">{{ t('ui.retry') }}</AppButton></AppCollectionState
    >
    <AppCollectionState
      v-else-if="!teams.data.value?.length"
      :title="t('teams.empty')"
      :description="t('teams.emptyHelp')"
      :icon="Users"
    />
    <AppPanel
      v-for="team in teams.data.value ?? []"
      v-else
      :key="team.name"
      :title="team.displayName || team.name"
      :description="team.name"
      compact
    >
      <div class="modern-teams-grant">
        <AppMultiSelect
          :model-value="selected(team)"
          class="modern-teams-groups"
          :label="t('teams.groups')"
          :placeholder="t('teams.selectGroups')"
          :options="groupOptions"
          :loading="groups.isFetching.value"
          :disabled="saving.has(team.name) || !groups.isSuccess.value"
          @update:model-value="select(team, $event)"
        />
        <AppButton
          variant="primary"
          size="sm"
          :loading="saving.has(team.name)"
          :disabled="!dirty(team) || !groups.isSuccess.value"
          @click="save(team)"
          >{{ t('teams.save') }}</AppButton
        >
      </div>
      <p v-if="!team.groupIDs.length" class="modern-teams-note">{{ t('teams.noGrants') }}</p>
    </AppPanel>
  </div>
</template>

<style scoped>
.modern-teams-workspace {
  display: grid;
  gap: var(--modern-space-4);
  align-content: start;
}
.modern-teams-help,
.modern-teams-note {
  margin: 0;
  color: var(--modern-muted);
  font-size: var(--modern-font-size-secondary);
}
.modern-teams-grant {
  display: flex;
  align-items: flex-end;
  gap: var(--modern-space-3);
}
.modern-teams-groups {
  flex: 1;
  min-width: 0;
}
</style>
