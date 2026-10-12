<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMessageSource } from '@modern/app/messages'
import { useI18n } from 'vue-i18n'
import type { GroupChannel, GroupCreateResult } from '@modern/api/group-create'
import type { CredentialStage } from '@modern/api/credential-stages'
import type { GroupRow } from '@modern/api/groups'
import { AppButton, AppNotice } from '@modern/components/ui'
import { useApiClient } from '@shared/http/client-context'
import {
  credentialEntriesInvalid,
  credentialFieldError,
  credentialEntriesPayload,
  filledCredentialEntries,
  maxCredentialEntries,
  type CredentialEntryDraft,
} from './group-create-rules'
import CredentialEntryList from './CredentialEntryList.vue'
import { useGroupCreateOperation } from './group-create-operation'
import GroupWorkspacePanel from './GroupWorkspacePanel.vue'
import SubscriptionCredentialStager from './SubscriptionCredentialStager.vue'

const props = defineProps<{ group: GroupRow; channel: GroupChannel }>()
const emit = defineEmits<{ close: []; saved: [result: GroupCreateResult] }>()
const { t, n, te } = useI18n()
const client = useApiClient()
const credentials = ref<CredentialEntryDraft[]>([])
const credentialList = ref<InstanceType<typeof CredentialEntryList>>()
const attempted = ref(false)
const completed = ref(false)
const stages = ref<CredentialStage[]>([])
const stageBusy = ref(false)
const stageDirty = ref(false)
const error = ref('')
const operation = useGroupCreateOperation(client)
const outcome = operation.outcome
const subscription = computed(() => props.group.connectionType === 'subscription')
const count = computed(() =>
  subscription.value
    ? stages.value.filter((stage) => stage.status === 'ready').length
    : filledCredentialEntries(credentials.value).length,
)
const awaiting = computed(() =>
  stages.value.some((stage) =>
    ['pending_authorization', 'exchanging', 'outcome_unknown'].includes(stage.status),
  ),
)
const uncertain = computed(() =>
  Boolean(outcome.value && !['success', 'rejected'].includes(outcome.value.kind)),
)
const locked = computed(() => operation.pending.value || uncertain.value)
const dirty = computed(
  () =>
    !completed.value &&
    Boolean(
      (!subscription.value && count.value) ||
      stages.value.length ||
      stageDirty.value ||
      operation.operation.value,
    ),
)
async function execute(): Promise<void> {
  const result = await operation.execute()
  if (!result) return
  if (result.kind === 'success') {
    completed.value = true
    credentials.value = []
    operation.reset()
    emit('saved', result.result)
    emit('close')
  } else if (result.kind === 'rejected') {
    const key = 'subscriptions.errors.' + result.error.code
    const fieldError = credentialFieldError(result.error)
    error.value = te(key)
      ? t(key)
      : fieldError
        ? t('groupCreate.credentialFieldError', fieldError)
        : t('groupDetail.importFailed')
    operation.reset()
  }
}
async function save(): Promise<void> {
  if (locked.value || stageBusy.value || awaiting.value || !count.value) return
  error.value = ''
  if (subscription.value) {
    stages.value = stages.value.map((stage) =>
      stage.status === 'ready' && stage.expiresAt <= Date.now()
        ? { ...stage, status: 'expired' }
        : stage,
    )
    const ready = stages.value.filter((stage) => stage.status === 'ready')
    if (!ready.length) {
      error.value = t('groupDetail.stageExpired')
      return
    }
    operation.begin({
      kind: 'connect',
      group: { id: props.group.id, name: props.group.name },
      stageIDs: ready.map((stage) => stage.id),
    })
  } else {
    attempted.value = true
    if (count.value > maxCredentialEntries || credentialEntriesInvalid(credentials.value)) {
      credentialList.value?.focus()
      return
    }
    // 去掉空行，使后端返回的条目序号与界面行号一致。
    credentials.value = filledCredentialEntries(credentials.value)
    operation.begin({
      kind: 'append',
      group: { id: props.group.id, name: props.group.name },
      credentials: credentialEntriesPayload(credentials.value),
    })
  }
  await execute()
}
useMessageSource(() => (error.value ? { text: error.value, tone: 'danger' } : undefined))
</script>

<template>
  <GroupWorkspacePanel
    :title="t(subscription ? 'groupDetail.connectAccount' : 'groupDetail.addCredentials')"
    :description="group.name"
    :dirty="dirty"
    :pending="operation.pending.value || stageBusy"
    :save-disabled="locked || awaiting || !count"
    :save-label="t('groupDetail.addCount', { count: n(count) })"
    @close="emit('close')"
    @save="save"
  >
    <SubscriptionCredentialStager
      v-if="subscription"
      v-model="stages"
      :group-id="group.id"
      :channel="channel"
      :disabled="locked"
      @busy="stageBusy = $event"
      @dirty="stageDirty = $event"
    />
    <CredentialEntryList
      v-else
      ref="credentialList"
      v-model="credentials"
      :channel="channel"
      :disabled="locked"
      :attempted="attempted"
      :error="
        attempted && count > maxCredentialEntries ? t('groupCreate.credentialsLimit') : undefined
      "
    />
    <AppNotice v-if="uncertain" tone="warning">
      {{ t('groupCreate.outcome.' + outcome!.kind) }}
      <template #actions
        ><AppButton
          v-if="outcome?.kind !== 'expired'"
          :disabled="!operation.canRetry.value"
          @click="execute"
          >{{ t('groupCreate.checkResult') }}</AppButton
        ></template
      >
    </AppNotice>
    <template #feedback
      ><span v-if="awaiting">{{ t('subscriptions.finishAuthorization') }}</span></template
    >
  </GroupWorkspacePanel>
</template>
