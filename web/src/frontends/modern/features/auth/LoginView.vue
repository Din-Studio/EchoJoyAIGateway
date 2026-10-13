<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { AppButton, AppNotice } from '@modern/components/ui'
import { loginURL } from '@modern/api/auth'
import { safeRedirect } from '@modern/app/redirect'
import AuthCard from './AuthCard.vue'
import LoginMascot from './LoginMascot.vue'

const route = useRoute()
const router = useRouter()
const { t } = useI18n()
const submitting = ref(false)
// 网关回调在失败时带回 error，只认这两个取值。
const loginError = computed(() =>
  route.query.error === 'no_access' || route.query.error === 'login_failed'
    ? route.query.error
    : undefined,
)
const message = computed(() => {
  if (loginError.value === 'no_access') return t('auth.noAccess')
  if (loginError.value === 'login_failed') return t('auth.loginFailed')
  return ''
})

watch(
  () => route.query,
  (query) => {
    const canonical: Record<string, string> = {}
    if (typeof query.redirect === 'string') canonical.redirect = query.redirect
    if (loginError.value) canonical.error = loginError.value
    if (
      Object.keys(query).length !== Object.keys(canonical).length ||
      Object.keys(canonical).some((key) => query[key] !== canonical[key])
    ) {
      void router.replace({ name: 'modern-login', query: canonical })
    }
  },
  { immediate: true },
)

function signIn(): void {
  submitting.value = true
  window.location.assign(loginURL(safeRedirect(route.query.redirect, router)))
}
</script>

<template>
  <div class="modern-login-stage">
    <LoginMascot :quiet="submitting" />
    <AuthCard
      class="modern-login-card"
      :title="t('auth.title')"
      :description="t('auth.description')"
    >
      <div class="modern-login-form">
        <AppNotice v-if="message" tone="danger">{{ message }}</AppNotice>
        <AppButton
          class="modern-login-submit"
          variant="primary"
          :loading="submitting"
          :disabled="submitting"
          @click="signIn"
        >
          {{ t(submitting ? 'auth.submitting' : 'auth.submit') }}
        </AppButton>
      </div>
    </AuthCard>
  </div>
</template>

<style scoped>
.modern-login-stage {
  --modern-login-mascot-width: 120px;
  --modern-login-mascot-seat: 0.435;
  position: relative;
  width: min(100%, 440px);
  padding-top: calc(var(--modern-login-mascot-width) * var(--modern-login-mascot-seat));
}
.modern-login-card {
  border-color: var(--modern-login-card-border);
  border-radius: var(--modern-radius-dialog);
  background: var(--modern-login-card-surface);
  padding-top: var(--modern-space-6);
  box-shadow: var(--modern-shadow-login);
}
.modern-login-form {
  display: grid;
  gap: var(--modern-space-4);
}
.modern-login-form .modern-login-submit {
  width: 100%;
}
@media (max-width: 760px) {
  .modern-login-stage {
    --modern-login-mascot-width: 108px;
  }
}
</style>
