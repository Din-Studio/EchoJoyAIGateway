<script setup lang="ts">
import { Trash2 } from '@lucide/vue'
import { useI18n } from 'vue-i18n'
import type { PriceField } from '@modern/api/models'
import { AppIconButton, AppTextField } from '@modern/components/ui'
import ModelPriceSlotFields from './ModelPriceSlotFields.vue'
import type { PriceDraftTier } from './model-price-draft'

// 一条阶梯价：输入 token 达到阈值后改用这一档的四个价格。
defineProps<{
  disabled: boolean
  error: (key: string) => string | undefined
}>()
defineEmits<{ remove: [] }>()
const tier = defineModel<PriceDraftTier>({ required: true })
const { t } = useI18n()
</script>

<template>
  <div class="modern-model-price-tier-editor">
    <header>
      <AppTextField
        :model-value="tier.threshold"
        :label="t('modelManager.threshold')"
        :error="error(tier.key)"
        :disabled="disabled"
        size="sm"
        inputmode="numeric"
        @update:model-value="tier = { ...tier, threshold: $event }"
      />
      <AppIconButton
        :icon="Trash2"
        :label="t('modelManager.removeTier')"
        :tooltip="true"
        size="xs"
        :disabled="disabled"
        @click="$emit('remove')"
      />
    </header>
    <ModelPriceSlotFields
      :model-value="tier.prices"
      :disabled="disabled"
      :error="(field: PriceField) => error(tier.key + '.' + field)"
      @update:model-value="tier = { ...tier, prices: $event }"
    />
    <p v-if="error(tier.key + '.prices')" class="modern-model-price-error">
      {{ error(tier.key + '.prices') }}
    </p>
  </div>
</template>

<style scoped>
.modern-model-price-tier-editor {
  display: grid;
  gap: var(--modern-space-3);
  border: var(--modern-line-width) solid var(--modern-border);
  border-radius: var(--modern-radius-control);
  padding: var(--modern-space-3);
}
/* 删除按钮固定在卡片右上角，阈值报错时也不会跟着下移。 */
.modern-model-price-tier-editor header {
  display: flex;
  align-items: flex-start;
  gap: var(--modern-space-3);
}
.modern-model-price-tier-editor header > :first-child {
  flex: 1;
  min-width: 0;
}
.modern-model-price-error {
  color: var(--modern-danger);
  font-size: var(--modern-font-size-small);
}
</style>
