<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { priceFields, type PriceField } from '@modern/api/models'
import { AppTextField } from '@modern/components/ui'

// 一组四个价格输入：标准档、模式档与阶梯档共用。
defineProps<{
  disabled: boolean
  error: (field: PriceField) => string | undefined
}>()
const prices = defineModel<Record<PriceField, string>>({ required: true })
const { t } = useI18n()
</script>

<template>
  <div class="modern-model-price-slots">
    <AppTextField
      v-for="field in priceFields"
      :key="field"
      :model-value="prices[field]"
      :label="t('modelManager.slots.' + field)"
      :error="error(field)"
      :disabled="disabled"
      :placeholder="t('modelManager.emptySlot')"
      inputmode="decimal"
      size="sm"
      @update:model-value="prices = { ...prices, [field]: $event }"
    />
  </div>
</template>

<style scoped>
.modern-model-price-slots {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--modern-space-3);
}
</style>
