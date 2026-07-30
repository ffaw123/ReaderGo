<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  ArrowRight,
  Check,
  Eye,
  EyeOff,
  Film,
  MessageCircleMore,
  Play,
  Sparkles,
} from 'lucide-vue-next'
import { api } from '../services/api'

const props = defineProps({ mode: { type: String, default: 'login' } })
const router = useRouter()
const form = reactive({ display_name: '', email: '', password: '', agreed: true })
const showPassword = ref(false)
const loading = ref(false)
const error = ref('')

const isRegister = computed(() => props.mode === 'register')
const title = computed(() => (isRegister.value ? '创建你的知识空间' : '欢迎回来'))
const subtitle = computed(() =>
  isRegister.value ? '注册后，开始让每一段视频都可检索、可追问。' : '继续探索你的视频知识库。',
)

watch(
  () => props.mode,
  () => {
    error.value = ''
  },
)

async function submit() {
  error.value = ''
  if (!form.email || !form.password) {
    error.value = '请填写邮箱和密码'
    return
  }
  if (form.password.length < 8) {
    error.value = '密码至少需要 8 个字符'
    return
  }
  if (isRegister.value && !form.agreed) {
    error.value = '请先阅读并同意服务条款'
    return
  }

  loading.value = true
  try {
    const result = isRegister.value
      ? await api.register({
          email: form.email,
          password: form.password,
          display_name: form.display_name,
        })
      : await api.login({ email: form.email, password: form.password })
    localStorage.setItem('readergo_session', JSON.stringify(result))
    sessionStorage.removeItem('readergo_preview')
    router.push('/app')
  } catch (requestError) {
    error.value = requestError.message
  } finally {
    loading.value = false
  }
}

function enterPreview() {
  sessionStorage.setItem('readergo_preview', 'true')
  router.push('/app')
}
</script>

<template>
  <main class="auth-page">
    <section class="auth-story">
      <div class="auth-story__noise"></div>
      <RouterLink class="brand brand--light" to="/login" aria-label="ReaderGo 首页">
        <span class="brand__mark"><Play :size="17" fill="currentColor" /></span>
        <span>ReaderGo</span>
      </RouterLink>

      <div class="story-copy">
        <span class="eyebrow eyebrow--light"><Sparkles :size="14" /> VIDEO INTELLIGENCE</span>
        <h1>别只看完视频。<br />真正读懂它。</h1>
        <p>把长视频变成可以搜索、追问和回看的知识，让重要信息不再从时间轴里溜走。</p>
        <div class="feature-row">
          <div>
            <span class="feature-icon"><Film :size="18" /></span>
            <strong>理解长视频</strong>
            <small>转写、画面与时间轴</small>
          </div>
          <div>
            <span class="feature-icon"><MessageCircleMore :size="18" /></span>
            <strong>基于内容问答</strong>
            <small>答案附带原片定位</small>
          </div>
        </div>
      </div>

      <blockquote class="story-quote">
        “一个让视频从内容，变成知识的工作台。”
        <footer><span>RG</span> ReaderGo 产品团队</footer>
      </blockquote>
    </section>

    <section class="auth-panel">
      <div class="auth-panel__mobile-brand">
        <span class="brand__mark"><Play :size="16" fill="currentColor" /></span>
        <strong>ReaderGo</strong>
      </div>
      <div class="auth-card">
        <div class="auth-heading">
          <span class="eyebrow">{{ isRegister ? 'START YOUR LIBRARY' : 'WELCOME BACK' }}</span>
          <h2>{{ title }}</h2>
          <p>{{ subtitle }}</p>
        </div>

        <form @submit.prevent="submit" novalidate>
          <label v-if="isRegister" class="field">
            <span>昵称 <small>选填</small></span>
            <input v-model.trim="form.display_name" autocomplete="name" placeholder="如何称呼你" />
          </label>
          <label class="field">
            <span>邮箱</span>
            <input v-model.trim="form.email" type="email" autocomplete="email" placeholder="name@example.com" />
          </label>
          <label class="field">
            <span>密码</span>
            <div class="input-with-action">
              <input
                v-model="form.password"
                :type="showPassword ? 'text' : 'password'"
                :autocomplete="isRegister ? 'new-password' : 'current-password'"
                placeholder="至少 8 个字符"
              />
              <button type="button" class="icon-button" @click="showPassword = !showPassword" aria-label="显示或隐藏密码">
                <EyeOff v-if="showPassword" :size="18" />
                <Eye v-else :size="18" />
              </button>
            </div>
          </label>

          <label v-if="isRegister" class="check-row">
            <input v-model="form.agreed" type="checkbox" />
            <span class="custom-check"><Check :size="12" /></span>
            <span>我已阅读并同意 <a href="#" @click.prevent>服务条款</a> 与 <a href="#" @click.prevent>隐私政策</a></span>
          </label>
          <div v-else class="form-meta">
            <label class="check-row">
              <input type="checkbox" checked />
              <span class="custom-check"><Check :size="12" /></span>
              <span>保持登录</span>
            </label>
            <a href="#" @click.prevent>忘记密码？</a>
          </div>

          <p v-if="error" class="form-error" role="alert">{{ error }}</p>
          <button class="primary-button auth-submit" :disabled="loading">
            <span>{{ loading ? '正在连接…' : isRegister ? '创建账户' : '登录' }}</span>
            <ArrowRight v-if="!loading" :size="18" />
          </button>
        </form>

        <p class="auth-switch">
          {{ isRegister ? '已经有账户？' : '还没有账户？' }}
          <RouterLink :to="isRegister ? '/login' : '/register'">
            {{ isRegister ? '直接登录' : '免费注册' }}
          </RouterLink>
        </p>

        <div class="preview-divider"><span>或</span></div>
        <button class="preview-button" type="button" @click="enterPreview">
          预览完整工作台
          <ArrowRight :size="16" />
        </button>
        <p class="preview-hint">无需连接后端，使用示例内容体验界面</p>
      </div>
      <p class="auth-footer">© 2026 ReaderGo · 让视频真正为你所用</p>
    </section>
  </main>
</template>
