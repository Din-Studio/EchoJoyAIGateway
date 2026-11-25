<script setup lang="ts">
import { computed, nextTick, onScopeDispose, ref, useId, watch } from 'vue'
import { ChevronDown, ChevronUp, X } from '@lucide/vue'
import { DialogRoot } from 'reka-ui'
import { useQuery } from '@tanstack/vue-query'
import { useI18n } from 'vue-i18n'
import { useApiClient } from '@shared/http/client-context'
import { ApiError } from '@shared/http/errors'
import { useMessages } from '@modern/app/messages'
import { useSystemCurrency } from '@modern/app/currency'
import {
  batchModelPrices,
  getPriceSources,
  priceSourcesKey,
  type ModelPrice,
  type ModelPriceSource,
  type PriceSchedules,
} from '@modern/api/models'
import {
  AppButton,
  AppCollectionState,
  AppConfirmDialog,
  AppDialogContent,
  AppDialogHeader,
  AppIconButton,
  AppNotice,
  AppOverflowText,
  AppSearchSelect,
  AppSegmentedControl,
} from '@modern/components/ui'
import { useLoadingActivity } from '@modern/components/ui/loading'
import AppDraftGuard from '@modern/components/AppDraftGuard.vue'
import ModelPriceForm from './ModelPriceForm.vue'
import ModelPricingDetails from './ModelPricingDetails.vue'
import ModelPriceSummary from './ModelPriceSummary.vue'
import ModelPriceTargets from './ModelPriceTargets.vue'
import { modelStateKeys } from './models-state'

const props = defineProps<{ targetId?: number }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { t, n } = useI18n()
const client = useApiClient()
const messages = useMessages()
const systemCurrency = useSystemCurrency()
const bulk = props.targetId === undefined
const targetsOpen = ref(bulk)
const targetsId = useId()
const ids = ref<number[]>(props.targetId === undefined ? [] : [props.targetId])
const initialIDs = JSON.stringify(ids.value)
const initialized = ref(false)
const mode = ref<'copy' | 'link'>('copy')
const selected = ref<ModelPriceSource>()
const copySource = ref<ModelPrice>()
const settingsTouched = ref(false)
const copyEditorKey = ref(0)
const edited = ref(false)
const pending = ref(false)
const unpricedOpen = ref(false)
const form = ref<InstanceType<typeof ModelPriceForm>>()
const body = ref<HTMLElement>()
const copyPrice = computed((): PriceSchedules | undefined => {
  if (copySource.value) return copySource.value
  const currency =
    targets.value[0]?.price.currency ??
    items.value[0]?.price.currency ??
    systemCurrency.value?.currency
  return currency
    ? {
        currency,
        prices: { input: null, output: null, cache_read: null, cache_write: null },
        context_tiers: [],
        mode_schedules: {},
      }
    : undefined
})
const dirty = computed(
  () => settingsTouched.value || edited.value || JSON.stringify(ids.value) !== initialIDs,
)
const guard = ref<InstanceType<typeof AppDraftGuard>>()
const copyFormId = useId()
const controller = new AbortController()
const query = useQuery({
  queryKey: priceSourcesKey,
  queryFn: ({ signal }) => getPriceSources(client, signal),
})
const items = computed(() => query.data.value ?? [])
const byID = computed(() => new Map(items.value.map((item) => [item.price.id, item])))
const targets = computed(() => items.value.filter((item) => ids.value.includes(item.price.id)))
// Each link points to its direct source. Visit each affected price once.
const affected = computed(() => {
  const children = new Map<number, number[]>()
  for (const { price } of items.value) {
    if (price.sourcePriceID === null) continue
    const siblings = children.get(price.sourcePriceID)
    if (siblings) siblings.push(price.id)
    else children.set(price.sourcePriceID, [price.id])
  }
  const result = new Set(ids.value)
  for (const id of result) {
    for (const child of children.get(id) ?? []) result.add(child)
  }
  return result
})
const groups = computed(
  () =>
    new Set(
      items.value
        .filter((s) => affected.value.has(s.price.id))
        .flatMap((s) => s.groups.map((g) => g.id)),
    ).size,
)
const modes = computed(() => [
  { value: 'copy', label: t('modelManager.reuse.copy') },
  { value: 'link', label: t('modelManager.reuse.link') },
])
const title = computed(() => t('modelManager.reuse.' + (bulk ? 'bulk' : 'title')))
const label = (source: ModelPriceSource) => `${source.price.channel.name} · ${source.price.model}`
function hasPrice(source: ModelPriceSource): boolean {
  return source.price.status === 'configured' && source.price.method !== 'user_marked_unpriced'
}
function existingReference(source: ModelPriceSource): boolean {
  return (
    ids.value.length > 0 &&
    targets.value.length === ids.value.length &&
    targets.value.every((target) => target.price.sourcePriceID === source.price.id)
  )
}
const canLink = computed(() =>
  Boolean(
    selected.value && hasPrice(selected.value) && !affected.value.has(selected.value.price.id),
  ),
)
const options = computed(() =>
  items.value
    .filter((item) => mode.value !== 'link' || !affected.value.has(item.price.id))
    .map((item) => ({
      value: String(item.price.id),
      label: label(item),
      keywords: [...item.aliases, ...item.groups.map((group) => group.name)],
      disabled: !hasPrice(item),
    })),
)
const selectedOption = computed(() =>
  selected.value
    ? { value: String(selected.value.price.id), label: label(selected.value) }
    : undefined,
)
const selectionValid = computed(
  () =>
    ids.value.length > 0 && ids.value.length <= 100 && targets.value.length === ids.value.length,
)
useLoadingActivity(() => query.isFetching.value || pending.value)
watch(
  [query.data, query.isFetching, query.isError],
  ([rows, fetching, failed]) => {
    if (!rows) return
    // Seed the editor from a fresh /sources response, never from a cached detail price.
    if (!initialized.value) {
      if (fetching || failed || !query.isFetchedAfterMount.value) return
      const initial =
        props.targetId === undefined ? undefined : byID.value.get(props.targetId)?.price
      copySource.value = initial
      if (initial?.sourcePriceID) {
        mode.value = 'link'
        selected.value = byID.value.get(initial.sourcePriceID)
      }
      initialized.value = true
    }
    if (!fetching && !failed) {
      const currentIDs = new Set(rows.map((item) => item.price.id))
      if (ids.value.some((id) => !currentIDs.has(id))) {
        ids.value = ids.value.filter((id) => currentIDs.has(id))
        messages.show({ tone: 'warning', text: t('modelManager.reuse.targetsChanged') })
      }
    }
    if (!fetching && !failed && mode.value === 'link' && selected.value) {
      const source = rows.find((item) => item.price.id === selected.value?.price.id)
      if (
        !source ||
        affected.value.has(source.price.id) ||
        (!hasPrice(source) && !existingReference(source))
      ) {
        selected.value = undefined
        messages.show({ tone: 'warning', text: t('modelManager.reuse.reselect') })
      } else selected.value = source
    }
  },
  { immediate: true },
)
watch(ids, () => {
  if (mode.value === 'link' && selected.value && affected.value.has(selected.value.price.id)) {
    selected.value = undefined
    messages.show({ tone: 'warning', text: t('modelManager.reuse.reselect') })
  }
})
async function select(key: string): Promise<void> {
  if (pending.value || key === String(selected.value?.price.id ?? '')) return
  if (!key) {
    selected.value = undefined
    settingsTouched.value = true
    return
  }
  const source = byID.value.get(Number(key))
  if (
    !source ||
    !hasPrice(source) ||
    (mode.value === 'link' && affected.value.has(source.price.id))
  )
    return
  if (edited.value && !(await guard.value?.confirm(false))) return
  selected.value = source
  settingsTouched.value = true
  if (mode.value === 'copy') {
    copySource.value = source.price
    copyEditorKey.value++
  }
  edited.value = false
}
async function changeMode(value: string): Promise<void> {
  if (pending.value || value === mode.value || (value !== 'copy' && value !== 'link')) return
  if (edited.value && !(await guard.value?.confirm(false))) return
  mode.value = value
  settingsTouched.value = true
  if (value === 'link' && selected.value && affected.value.has(selected.value.price.id))
    selected.value = undefined
  if (value === 'copy') {
    copySource.value =
      selected.value?.price ?? (ids.value.length === 1 ? targets.value[0]?.price : undefined)
    copyEditorKey.value++
  }
  edited.value = false
}
async function close(): Promise<void> {
  if (!pending.value && (await guard.value?.confirm())) emit('close')
}
async function apply(confirmed = false): Promise<void> {
  if (pending.value || !selectionValid.value) return
  const source = selected.value
  if (mode.value === 'copy') {
    if (!form.value?.validate()) {
      await nextTick()
      body.value?.querySelector<HTMLInputElement>('[aria-invalid="true"]')?.focus()
      return
    }
    if (form.value.unpricedRequested && !confirmed) {
      unpricedOpen.value = true
      return
    }
  } else if (!source || !canLink.value) return
  pending.value = true
  try {
    await batchModelPrices(
      client,
      mode.value === 'copy'
        ? { ids: [...ids.value], mode: 'copy', price: form.value!.request }
        : { ids: [...ids.value], mode: 'link', reference: { price_id: source!.price.id } },
      controller.signal,
    )
    if (!controller.signal.aborted) {
      edited.value = false
      settingsTouched.value = false
      unpricedOpen.value = false
      messages.show({ tone: 'success', text: t('modelManager.saveSuccess') })
      emit('saved')
    }
  } catch (cause) {
    if (!controller.signal.aborted)
      messages.show({
        tone: 'danger',
        text: cause instanceof ApiError ? cause.message : t('modelManager.saveFailed'),
      })
  } finally {
    pending.value = false
  }
}
onScopeDispose(() => controller.abort())
</script>

<template>
  <DialogRoot :open="true" @update:open="!$event && close()">
    <AppDialogContent
      size="wide"
      :title="title"
      :description="t('modelManager.reuse.chooseTargets')"
      class="modern-price-setup-dialog"
      @escape-key-down="pending && $event.preventDefault()"
      @interact-outside="pending && $event.preventDefault()"
    >
      <AppDialogHeader
        :title="title"
        :close-label="t('modelManager.reuse.close')"
        :close-disabled="pending"
        @close="close"
      />
      <div v-if="!initialized" class="modern-price-setup-loading">
        <AppCollectionState
          :loading="query.isFetching.value"
          :error="query.isError.value"
          :title="t(query.isError.value ? 'modelManager.failed' : 'modelManager.loading')"
        >
          <AppButton v-if="query.isError.value" @click="query.refetch()">{{
            t('ui.retry')
          }}</AppButton>
        </AppCollectionState>
      </div>
      <div v-else class="modern-price-setup-workspace">
        <section
          class="modern-price-setup-selection"
          :class="{ 'is-collapsed': !targetsOpen }"
          :aria-label="t('modelManager.reuse.chooseTargets')"
        >
          <header class="modern-price-setup-selection-header">
            <h3>{{ t('modelManager.reuse.chooseTargets') }}</h3>
            <AppButton
              class="modern-price-setup-toggle"
              variant="ghost"
              :icon="targetsOpen ? ChevronUp : ChevronDown"
              :aria-expanded="targetsOpen"
              :aria-controls="targetsId"
              @click="targetsOpen = !targetsOpen"
            >
              {{ t('modelManager.reuse.' + (targetsOpen ? 'collapseTargets' : 'chooseTargets')) }}
            </AppButton>
            <span
              v-if="!targetsOpen"
              class="modern-price-setup-selection-summary"
              aria-live="polite"
            >
              <AppOverflowText v-if="targets.length === 1" :text="label(targets[0]!)" />
              <template v-else>{{
                t('modelManager.reuse.selected', { count: n(ids.length) })
              }}</template>
            </span>
          </header>
          <div :id="targetsId" class="modern-price-setup-targets">
            <ModelPriceTargets
              v-model="ids"
              :items="items"
              :loading="query.isFetching.value"
              :disabled="pending"
            />
          </div>
        </section>
        <section class="modern-price-setup-settings" :aria-label="t('modelManager.reuse.title')">
          <div class="modern-price-setup-controls">
            <AppNotice v-if="query.isError.value" tone="warning">
              {{ t('modelManager.stale') }}
              <AppButton
                variant="text"
                size="xs"
                :disabled="query.isFetching.value"
                @click="query.refetch()"
                >{{ t('ui.retry') }}</AppButton
              >
            </AppNotice>
            <div class="modern-price-setup-method">
              <h3>{{ t('modelManager.reuse.title') }}</h3>
              <AppSegmentedControl
                :model-value="mode"
                :options="modes"
                :label="t('modelManager.reuse.title')"
                appearance="field"
                size="sm"
                :disabled="pending"
                @update:model-value="changeMode"
              />
            </div>
            <div class="modern-price-setup-source">
              <AppSearchSelect
                :model-value="selected ? String(selected.price.id) : ''"
                :selected-option="selectedOption"
                :label="t('modelManager.reuse.' + (mode === 'copy' ? 'sourceOptional' : 'source'))"
                :placeholder="t('modelManager.reuse.' + (mode === 'copy' ? 'search' : 'select'))"
                size="sm"
                :options="options"
                :disabled="pending"
                @update:model-value="select"
              >
                <template #option="{ option }">
                  <span class="modern-price-source-option">
                    <span class="modern-price-source-identity">
                      <AppOverflowText
                        :text="byID.get(Number(option.value))?.price.model ?? option.label"
                      />
                      <small v-if="byID.get(Number(option.value))"
                        ><AppOverflowText
                          :text="byID.get(Number(option.value))!.price.channel.name"
                      /></small>
                    </span>
                    <ModelPriceSummary
                      v-if="byID.get(Number(option.value))"
                      :prices="byID.get(Number(option.value))!.price.prices"
                      :currency="byID.get(Number(option.value))!.price.currency"
                    />
                  </span>
                </template>
              </AppSearchSelect>
              <AppIconButton
                v-if="mode === 'copy' && selected"
                :icon="X"
                :label="t('modelManager.reuse.clearReference')"
                :disabled="pending"
                @click="select('')"
              />
            </div>
            <p v-if="mode === 'link'" class="modern-price-setup-hint">
              {{ t('modelManager.reuse.linkHint') }}
            </p>
            <AppNotice v-if="mode === 'link' && selected && !hasPrice(selected)" tone="warning">{{
              t('modelManager.reuse.sourceUnpriced')
            }}</AppNotice>
          </div>
          <div v-if="mode === 'copy' || selected" ref="body" class="modern-price-setup-prices">
            <form
              v-if="mode === 'copy' && copyPrice"
              :id="copyFormId"
              novalidate
              @submit.prevent="apply()"
            >
              <ModelPriceForm
                ref="form"
                :key="copyEditorKey"
                :price="copyPrice"
                :unpriced-allowed="true"
                :method="copySource?.method"
                :disabled="pending || !selectionValid"
                @dirty="edited = $event"
              />
            </form>
            <template v-else-if="selected">
              <ModelPricingDetails :price="selected.price" />
            </template>
          </div>
          <p v-if="mode === 'copy'" class="modern-price-setup-replacement">
            {{ t('modelManager.reuse.replaceHint') }}
          </p>
        </section>
      </div>
      <footer class="modern-price-setup-footer">
        <div class="modern-price-setup-impact" aria-live="polite">
          <span>{{ t('modelManager.reuse.selected', { count: n(ids.length) }) }}</span>
          <small v-if="ids.length">{{
            t(
              'modelManager.reuse.' +
                (affected.size > ids.length ? 'scopeDependents' : 'scopeGroups'),
              { count: n(affected.size - ids.length), groups: n(groups) },
            )
          }}</small>
        </div>
        <div class="modern-price-setup-actions">
          <AppButton size="sm" :disabled="pending" @click="close">{{ t('ui.cancel') }}</AppButton>
          <AppButton
            size="sm"
            :type="mode === 'copy' ? 'submit' : 'button'"
            :form="mode === 'copy' ? copyFormId : undefined"
            variant="primary"
            :disabled="!selectionValid || pending || (mode === 'link' && !canLink)"
            :loading="pending"
            @click="mode === 'link' && apply()"
            >{{
              ids.length
                ? t('modelManager.reuse.apply', { count: n(ids.length) })
                : t('modelManager.save')
            }}</AppButton
          >
        </div>
      </footer>
      <AppConfirmDialog
        :open="unpricedOpen"
        :title="t('modelManager.unpricedTitle', { count: n(ids.length) })"
        :description="t('modelManager.unpricedDescription')"
        :confirm-label="t('modelManager.confirmUnpriced')"
        :pending="pending"
        @confirm="apply(true)"
        @cancel="unpricedOpen = false"
      />
      <AppDraftGuard ref="guard" :dirty="dirty" :pending="pending" :query-scope="modelStateKeys" />
    </AppDialogContent>
  </DialogRoot>
</template>

<style scoped>
.modern-price-setup-loading {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
}
.modern-price-setup-workspace {
  display: grid;
  grid-template-columns: minmax(0, 1.1fr) minmax(0, 1fr);
  flex: 1;
  min-height: 0;
}
.modern-price-setup-selection {
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  border-right: var(--modern-line-width) solid var(--modern-border);
}
.modern-price-setup-selection-header {
  display: flex;
  min-height: calc(var(--modern-control-sm) + var(--modern-space-4));
  flex: none;
  align-items: center;
  gap: var(--modern-space-3);
  padding: var(--modern-space-4) var(--modern-space-4) 0;
}
.modern-price-setup-selection-header h3 {
  flex: none;
  margin: 0;
  font-size: var(--modern-font-size-secondary);
  font-weight: var(--modern-weight-semibold);
}
.modern-price-setup-selection-summary {
  display: none;
  min-width: 0;
  color: var(--modern-muted);
  font-size: var(--modern-font-size-small);
}
.modern-price-setup-toggle {
  display: none;
}
.modern-price-setup-targets {
  display: flex;
  flex: 1;
  min-height: 0;
}
.modern-price-setup-settings {
  min-width: 0;
  min-height: 0;
  overflow-y: auto;
  scrollbar-gutter: var(--modern-scrollbar-gutter);
  padding: var(--modern-space-4);
  overscroll-behavior: contain;
}
.modern-price-setup-controls {
  display: grid;
  gap: var(--modern-space-3);
}
.modern-price-setup-method {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: var(--modern-space-2);
}
.modern-price-setup-method h3 {
  margin: 0;
  font-size: var(--modern-font-size-secondary);
  font-weight: var(--modern-weight-semibold);
}
.modern-price-setup-source {
  display: flex;
  align-items: flex-end;
  gap: var(--modern-space-2);
}
.modern-price-setup-source > :first-child {
  flex: 1;
  min-width: 0;
}
.modern-price-source-option {
  display: flex;
  flex: 1;
  min-width: 0;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: var(--modern-space-1) var(--modern-space-3);
}
.modern-price-source-option > :first-child {
  flex: 1;
  min-width: 140px;
}
.modern-price-source-identity {
  display: flex;
  align-items: baseline;
  gap: var(--modern-space-2);
}
.modern-price-source-identity > :first-child {
  flex: 1;
  min-width: 0;
}
.modern-price-source-identity small {
  max-width: 40%;
  min-width: 0;
  color: var(--modern-muted);
  font-size: var(--modern-font-size-small);
}
.modern-price-setup-hint {
  margin: 0;
  font-size: var(--modern-font-size-small);
  color: var(--modern-muted);
}
.modern-price-setup-prices {
  margin-top: var(--modern-space-4);
}
.modern-price-setup-replacement {
  margin: var(--modern-space-3) 0 0;
  color: var(--modern-muted);
  font-size: var(--modern-font-size-small);
}
.modern-price-setup-footer {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: space-between;
  gap: var(--modern-space-4);
  padding: var(--modern-space-3) var(--modern-space-4);
  border-top: var(--modern-line-width) solid var(--modern-border);
}
.modern-price-setup-impact {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  min-width: 0;
  gap: var(--modern-space-1) var(--modern-space-3);
  font-size: var(--modern-font-size-secondary);
}
.modern-price-setup-impact small {
  color: var(--modern-muted);
  font-size: var(--modern-font-size-small);
}
.modern-price-setup-actions {
  display: flex;
  flex: none;
  gap: var(--modern-space-2);
}
@media (max-width: 1150px) {
  .modern-price-setup-workspace {
    display: block;
    overflow-y: auto;
    overscroll-behavior: contain;
  }
  .modern-price-setup-selection {
    border-right: 0;
    border-bottom: var(--modern-line-width) solid var(--modern-border);
  }
  .modern-price-setup-selection-header {
    padding-bottom: var(--modern-space-3);
  }
  .modern-price-setup-selection-header h3 {
    display: none;
  }
  .modern-price-setup-selection-summary {
    display: block;
  }
  .modern-price-setup-toggle {
    display: inline-flex;
    flex: none;
  }
  .modern-price-setup-targets {
    flex: none;
    height: min(360px, 50dvh);
    min-height: 280px;
  }
  .modern-price-setup-selection.is-collapsed .modern-price-setup-targets {
    display: none;
  }
  .modern-price-setup-settings {
    overflow: visible;
  }
}
@media (max-width: 760px) {
  .modern-price-setup-footer {
    flex-wrap: wrap;
    gap: var(--modern-space-3);
  }
  .modern-price-setup-impact,
  .modern-price-setup-actions {
    width: 100%;
  }
  .modern-price-setup-actions > :last-child {
    flex: 1;
  }
}
</style>

<!-- Portal 的尺寸规则直接匹配该弹窗，不依赖父组件 scoped 属性。 -->
<style>
.modern-dialog.modern-price-setup-dialog {
  --modern-dialog-wide-width: 1100px;
  top: 50%;
  height: min(760px, calc(100dvh - var(--modern-space-8)));
  max-height: calc(100dvh - var(--modern-space-8));
  transform: translate(-50%, -50%);
}
@media (max-width: 760px) {
  .modern-dialog.modern-price-setup-dialog {
    inset: 0;
    width: 100%;
    height: 100dvh;
    max-height: 100dvh;
    transform: none;
  }
}
</style>
