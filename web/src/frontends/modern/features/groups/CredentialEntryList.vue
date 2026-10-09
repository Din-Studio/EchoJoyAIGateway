<script setup lang="ts">
import { Plus, Trash2 } from '@lucide/vue'
import { computed, nextTick, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { GroupChannel } from '@modern/api/group-create'
import { AppButton, AppIconButton, AppTextField } from '@modern/components/ui'
import {
  concurrencyLimitInvalid,
  filledCredentialEntries,
  maxCredentialEntries,
  type CredentialEntryDraft,
} from './group-create-rules'

const props = defineProps<{
  channel: GroupChannel
  disabled?: boolean
  attempted?: boolean
  error?: string
}>()
const entries = defineModel<CredentialEntryDraft[]>({ required: true })
const { t } = useI18n()
const labelID = useId()
const fields = computed(() => props.channel.credentialFields)
// 只有单个 api_key 字段时按行拆分粘贴内容；结构化字段（如 JSON）保持原样。
const plainKeys = computed(() => fields.value.length === 1 && fields.value[0]?.key === 'api_key')
const pasteError = ref('')
const inputs = new Map<string, { focus(): void }>()

function blank(key: number, concurrencyLimit: string, apiKey = ''): CredentialEntryDraft {
  return {
    key,
    fields: Object.fromEntries(
      fields.value.map((field) => [field.key, field.key === 'api_key' ? apiKey : '']),
    ),
    concurrencyLimit,
  }
}
function nextKey(): number {
  return Math.max(0, ...entries.value.map((entry) => entry.key)) + 1
}
function filled(entry: CredentialEntryDraft): boolean {
  return Object.values(entry.fields).some((value) => value.trim())
}
watch(
  entries,
  (value) => {
    if (!value.length) entries.value = [blank(1, '')]
  },
  { immediate: true },
)
function inputRef(id: string, element: unknown): void {
  if (element && typeof element === 'object' && 'focus' in element)
    inputs.set(id, element as { focus(): void })
  else inputs.delete(id)
}
function focusEntry(entry: CredentialEntryDraft | undefined): void {
  const field = fields.value[0]
  if (entry && field) inputs.get(`${entry.key}:${field.key}`)?.focus()
}
function update(key: number, field: string, value: string): void {
  pasteError.value = ''
  entries.value = entries.value.map((entry) =>
    entry.key === key ? { ...entry, fields: { ...entry.fields, [field]: value } } : entry,
  )
}
function updateLimit(key: number, value: string): void {
  entries.value = entries.value.map((entry) =>
    entry.key === key ? { ...entry, concurrencyLimit: value.trim() } : entry,
  )
}
async function add(): Promise<void> {
  const last = entries.value.at(-1)
  const entry = blank(nextKey(), last?.concurrencyLimit ?? '')
  entries.value = [...entries.value, entry]
  await nextTick()
  focusEntry(entry)
}
function remove(key: number): void {
  pasteError.value = ''
  entries.value = entries.value.filter((entry) => entry.key !== key)
}
// 多行粘贴拆成多条，新行沿用当前行的并发上限。
function paste(event: ClipboardEvent, target: CredentialEntryDraft): void {
  if (!plainKeys.value) return
  const lines = (event.clipboardData?.getData('text') ?? '')
    .split(/\r?\n/u)
    .map((line) => line.trim())
    .filter(Boolean)
  if (lines.length < 2) return
  event.preventDefault()
  const reuse = !filled(target)
  const added = reuse ? lines.slice(1) : lines
  if (filledCredentialEntries(entries.value).length + lines.length > maxCredentialEntries) {
    pasteError.value = t('groupCreate.credentialsLimit')
    return
  }
  pasteError.value = ''
  const base = nextKey()
  const created = added.map((line, index) => blank(base + index, target.concurrencyLimit, line))
  entries.value = entries.value.flatMap((entry) =>
    entry.key !== target.key
      ? [entry]
      : [reuse ? { ...entry, fields: { ...entry.fields, api_key: lines[0]! } } : entry, ...created],
  )
}
defineExpose({
  focus(): void {
    const invalid = entries.value.find(
      (entry) => filled(entry) && concurrencyLimitInvalid(entry.concurrencyLimit),
    )
    if (invalid) inputs.get(`${invalid.key}:concurrency`)?.focus()
    else focusEntry(entries.value.find((entry) => !filled(entry)) ?? entries.value[0])
  },
})
</script>

<template>
  <div class="modern-credential-entries" role="group" :aria-labelledby="labelID">
    <span :id="labelID" class="modern-credential-entries-title">{{
      t('groupCreate.credentials')
    }}</span>
    <p class="modern-credential-entries-description">
      {{ t(plainKeys ? 'groupCreate.credentialsHelp' : 'groupCreate.structuredHelp') }}
    </p>
    <div v-if="plainKeys" class="modern-credential-entry" aria-hidden="true">
      <span class="modern-credential-entries-label">{{ fields[0]?.label }}</span>
      <span class="modern-credential-entries-label">{{ t('groups.edit.concurrencyLimit') }}</span>
    </div>
    <div
      v-for="(entry, index) in entries"
      :key="entry.key"
      class="modern-credential-entry"
      :class="{ 'is-structured': !plainKeys }"
    >
      <div class="modern-credential-entry-fields">
        <AppTextField
          v-for="field in fields"
          :key="field.key"
          :ref="(element) => inputRef(`${entry.key}:${field.key}`, element)"
          :model-value="entry.fields[field.key] ?? ''"
          :label="
            plainKeys
              ? t('groupCreate.credentialEntry', { entry: index + 1, field: field.label })
              : field.label
          "
          :label-hidden="plainKeys"
          :placeholder="plainKeys ? t('groupCreate.credentialsPlaceholder') : undefined"
          :disabled="disabled"
          autocomplete="off"
          spellcheck="false"
          size="sm"
          @update:model-value="update(entry.key, field.key, $event)"
          @paste="paste($event, entry)"
        />
      </div>
      <AppTextField
        :ref="(element) => inputRef(`${entry.key}:concurrency`, element)"
        :model-value="entry.concurrencyLimit"
        :label="
          plainKeys
            ? t('groupCreate.credentialEntry', {
                entry: index + 1,
                field: t('groups.edit.concurrencyLimit'),
              })
            : t('groups.edit.concurrencyLimit')
        "
        :label-hidden="plainKeys"
        :placeholder="t('groups.edit.concurrencyUnlimited')"
        :disabled="disabled"
        :error="
          attempted && filled(entry) && concurrencyLimitInvalid(entry.concurrencyLimit)
            ? t('groups.edit.concurrencyLimitError')
            : undefined
        "
        inputmode="numeric"
        autocomplete="off"
        size="sm"
        @update:model-value="updateLimit(entry.key, $event)"
      />
      <AppIconButton
        :icon="Trash2"
        :label="t('groupCreate.removeCredential', { entry: index + 1 })"
        size="sm"
        :disabled="disabled || (entries.length === 1 && !filled(entry))"
        @click="remove(entry.key)"
      />
    </div>
    <div>
      <AppButton
        :icon="Plus"
        variant="ghost"
        size="sm"
        :disabled="disabled || entries.length >= maxCredentialEntries"
        @click="add"
        >{{ t('groupCreate.addCredential') }}</AppButton
      >
    </div>
    <p v-if="pasteError || error" class="modern-credential-entries-error" role="alert">
      {{ pasteError || error }}
    </p>
  </div>
</template>

<style scoped>
.modern-credential-entries {
  display: grid;
  gap: var(--modern-space-2);
}
.modern-credential-entries-title {
  color: var(--modern-text);
  font-size: var(--modern-font-size-secondary);
  font-weight: var(--modern-weight-medium);
  line-height: var(--modern-leading-compact);
}
.modern-credential-entries-description,
.modern-credential-entries-label {
  color: var(--modern-muted);
  font-size: var(--modern-font-size-small);
}
.modern-credential-entries-error {
  color: var(--modern-danger);
  font-size: var(--modern-font-size-small);
}
.modern-credential-entry {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 9rem) var(--modern-control-sm);
  gap: var(--modern-space-2);
  align-items: start;
}
.modern-credential-entry.is-structured {
  align-items: end;
  border-bottom: var(--modern-line-width) solid var(--modern-border);
  padding-bottom: var(--modern-space-3);
}
.modern-credential-entry-fields {
  display: grid;
  gap: var(--modern-space-2);
}
.modern-credential-entry.is-structured .modern-credential-entry-fields {
  grid-column: 1 / -1;
}
.modern-credential-entry.is-structured > :nth-child(2) {
  grid-column: 1 / 3;
}
@media (max-width: 760px) {
  .modern-credential-entry {
    grid-template-columns: minmax(0, 1fr) minmax(0, 6rem) var(--modern-touch-target);
  }
}
</style>
