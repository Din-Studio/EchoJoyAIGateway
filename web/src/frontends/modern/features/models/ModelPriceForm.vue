<script setup lang="ts">
import { Info, Plus, Trash2 } from '@lucide/vue'
import { computed, onScopeDispose, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  priceFields,
  type ModelPrice,
  type PriceField,
  type PriceSchedules,
} from '@modern/api/models'
import { AppButton, AppIcon, AppIconButton, AppTooltip } from '@modern/components/ui'
import ModelPriceSlotFields from './ModelPriceSlotFields.vue'
import ModelPriceTierEditor from './ModelPriceTierEditor.vue'
import {
  draftSlots,
  newPriceTier,
  priceDraft,
  priceDraftErrors,
  priceDraftRequest,
} from './model-price-draft'

// 价格表单直接嵌在模型详情里：只管草稿与校验，保存由详情页统一发起。
// 只有上游价格允许全部留空并确认为不计费；统一价未设置即待定价，至少要填一项基础价格。
const props = defineProps<{
  price: PriceSchedules
  unpricedAllowed: boolean
  method?: ModelPrice['method']
  disabled: boolean
}>()
const emit = defineEmits<{ dirty: [value: boolean] }>()
const { t } = useI18n()
const draft = ref(priceDraft(props.price))
const baseline = ref(JSON.stringify(draft.value))
const dirty = computed(() => JSON.stringify(draft.value) !== baseline.value)
const submitted = ref(false)
const request = computed(() => priceDraftRequest(draft.value))
const errors = computed(() => {
  const result = priceDraftErrors(draft.value)
  const standard = draft.value.find((schedule) => schedule.mode === 'standard')!
  if (!props.unpricedAllowed && priceFields.every((field) => !standard.prices[field].trim()))
    result.standard = 'emptyBase'
  return result
})
// 全部留空即标记为不计费；已经标记过的价格不必再次保存。
const unpricedRequested = computed(() => props.unpricedAllowed && request.value.confirm_unpriced)
const canSave = computed(
  () => dirty.value || (unpricedRequested.value && props.method !== 'user_marked_unpriced'),
)
const error = (key: string) =>
  submitted.value && errors.value[key] ? t('modelManager.' + errors.value[key]) : undefined
// standard 固定存在；这两档后端一视同仁，前端也对称提供。
const optionalModes = ['fast', 'ultrafast'] as const
const modeLabel = (mode: string) =>
  ['standard', 'fast', 'ultrafast'].includes(mode) ? t('modelManager.' + mode) : mode

watch(dirty, (value) => emit('dirty', value), { immediate: true })
watch(
  () => props.price,
  (price) => {
    if (dirty.value) return
    draft.value = priceDraft(price)
    baseline.value = JSON.stringify(draft.value)
  },
)
onScopeDispose(() => emit('dirty', false))
// 显示全部错误并返回是否可以提交。
function validate(): boolean {
  submitted.value = true
  return Object.keys(errors.value).length === 0
}
defineExpose({ canSave, unpricedRequested, request, validate })
</script>

<template>
  <div class="modern-model-price-form">
    <div class="modern-model-price-form-hint">
      <span>{{ t('modelManager.unit', { currency: price.currency }) }}</span>
      <AppTooltip :label="t('modelManager.emptySlotsHint')">
        <span tabindex="0" class="modern-model-price-help"><AppIcon :icon="Info" size="sm" /></span>
      </AppTooltip>
    </div>
    <div v-for="schedule in draft" :key="schedule.mode" class="modern-model-price-schedule">
      <header v-if="draft.length > 1">
        <strong>{{ modeLabel(schedule.mode) }}</strong>
        <template v-if="schedule.mode !== 'standard'">
          <!-- 阶梯价只在标准档提供：Fast 后端直接驳回，Ultrafast 统一不做。 -->
          <span class="modern-model-price-note">{{ t('modelManager.noTierMode') }}</span>
          <AppIconButton
            :icon="Trash2"
            :label="t('modelManager.removeMode')"
            :tooltip="true"
            size="xs"
            :disabled="disabled"
            @click="draft = draft.filter((item) => item !== schedule)"
          />
        </template>
      </header>
      <ModelPriceSlotFields
        v-model="schedule.prices"
        :disabled="disabled"
        :error="(field: PriceField) => error(schedule.mode + '.' + field)"
      />
      <p v-if="error(schedule.mode)" class="modern-model-price-error">
        {{ error(schedule.mode) }}
      </p>
      <ModelPriceTierEditor
        v-for="(tier, index) in schedule.tiers"
        :key="tier.key"
        v-model="schedule.tiers[index]!"
        :disabled="disabled"
        :error="error"
        @remove="schedule.tiers.splice(index, 1)"
      />
      <AppButton
        v-if="schedule.mode === 'standard'"
        class="modern-model-price-add"
        variant="ghost"
        size="sm"
        :icon="Plus"
        :disabled="disabled"
        @click="schedule.tiers.push(newPriceTier())"
      >
        {{ t('modelManager.addTier') }}
      </AppButton>
    </div>
    <div
      v-if="optionalModes.some((mode) => !draft.some((row) => row.mode === mode))"
      class="modern-model-price-modes"
    >
      <AppButton
        v-for="mode in optionalModes.filter((item) => !draft.some((row) => row.mode === item))"
        :key="mode"
        variant="ghost"
        size="sm"
        :icon="Plus"
        :disabled="disabled"
        @click="draft.push({ mode, prices: draftSlots(), tiers: [] })"
      >
        {{ t('modelManager.addMode') }} · {{ t('modelManager.' + mode) }}
      </AppButton>
    </div>
  </div>
</template>

<style scoped>
.modern-model-price-form {
  display: grid;
  gap: var(--modern-space-4);
  min-width: 0;
}
.modern-model-price-form-hint {
  display: flex;
  align-items: center;
  gap: var(--modern-space-2);
  color: var(--modern-muted);
  font-size: var(--modern-font-size-small);
}
.modern-model-price-help {
  display: inline-flex;
  border-radius: var(--modern-radius-small);
}
.modern-model-price-schedule {
  display: grid;
  gap: var(--modern-space-3);
  min-width: 0;
}
.modern-model-price-schedule + .modern-model-price-schedule {
  padding-top: var(--modern-space-4);
  border-top: var(--modern-line-width) dashed var(--modern-border);
}
.modern-model-price-schedule header {
  display: flex;
  align-items: center;
  gap: var(--modern-space-2);
}
.modern-model-price-schedule header strong {
  font-size: var(--modern-font-size-small);
  font-weight: var(--modern-weight-semibold);
}
.modern-model-price-note {
  flex: 1;
  color: var(--modern-muted);
  font-size: var(--modern-font-size-caption);
}
.modern-model-price-add {
  justify-self: start;
}
.modern-model-price-modes {
  display: flex;
  flex-wrap: wrap;
  gap: var(--modern-space-2);
}
.modern-model-price-error {
  color: var(--modern-danger);
  font-size: var(--modern-font-size-small);
}
</style>
