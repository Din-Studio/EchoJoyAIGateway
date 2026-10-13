<script setup lang="ts">
import { LoaderCircle } from '@lucide/vue'
import { computed, nextTick, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { isNavigationFailure, useRoute, useRouter } from 'vue-router'

import { AppButton, AppIcon, AppNotice } from '@modern/components/ui'
import PublicLayout from '@modern/layouts/PublicLayout.vue'
import { loginLocation } from '@modern/app/redirect'
import AuthCard from './AuthCard.vue'
import { useAuthSession } from './auth-session'

const session = useAuthSession()
const route = useRoute()
const router = useRouter()
const { t } = useI18n()
const actions = ref<HTMLElement>()
const navigationFailed = ref(false)
const canRender = computed(
  () => session.state.phase === 'validated' && (!route.meta.adminOnly || session.state.platform),
)
const message = computed(() => {
  if (navigationFailed.value) return t('shell.navigationFailed')
  if (session.state.phase === 'network-error') return t('auth.network')
  if (session.state.phase === 'invalid-response') return t('auth.invalidResponse')
  return ''
})

async function navigate(
  target: ReturnType<typeof loginLocation> | { name: string },
): Promise<void> {
  try {
    const failure = await router.replace(target)
    navigationFailed.value = isNavigationFailure(failure)
  } catch {
    navigationFailed.value = true
  }
}

watch(
  [() => session.state.phase, () => session.state.platform, () => route.fullPath],
  ([phase, platform]) => {
    if (phase === 'unvalidated') void session.ensureValidated()
    if (phase === 'anonymous') void navigate(loginLocation(route.fullPath))
    if (phase === 'validated' && !platform && route.meta.adminOnly) {
      void navigate({ name: 'modern-home' })
    }
  },
  { immediate: true },
)
watch(
  () => session.state.phase,
  async (phase) => {
    if (phase !== 'network-error' && phase !== 'invalid-response') return
    await nextTick()
    actions.value?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus()
  },
)

function retry(): void {
  void session.ensureValidated()
}
function reload(): void {
  window.location.reload()
}
</script>

<template>
  <slot v-if="canRender" />
  <PublicLayout v-else restoring-session>
    <AuthCard :title="t('auth.restoreTitle')">
      <template v-if="message">
        <AppNotice tone="danger">{{ message }}</AppNotice>
        <div ref="actions" class="modern-auth-gate-actions">
          <AppButton v-if="navigationFailed" @click="reload">{{ t('shell.reload') }}</AppButton>
          <AppButton v-else @click="retry">{{ t('auth.retry') }}</AppButton>
          <AppButton variant="ghost" @click="session.clear()">{{ t('auth.signIn') }}</AppButton>
        </div>
      </template>
      <div v-else class="modern-auth-checking" role="status">
        <AppIcon :icon="LoaderCircle" class="modern-spin" />
        <span>{{ t('auth.checking') }}</span>
      </div>
    </AuthCard>
  </PublicLayout>
</template>

<style scoped>
.modern-auth-checking,
.modern-auth-gate-actions {
  display: flex;
  align-items: center;
  gap: var(--modern-space-2);
}
.modern-auth-checking {
  color: var(--modern-muted);
  font-size: var(--modern-font-size-secondary);
}
.modern-auth-gate-actions {
  flex-wrap: wrap;
}
</style>
