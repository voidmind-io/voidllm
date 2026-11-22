import { useEffect } from 'react'
import { storeSession } from '../../lib/session'

function getCookie(name: string): string | null {
  const match = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'))
  return match ? decodeURIComponent(match[1]) : null
}

export default function CallbackPage() {
  useEffect(() => {
    const token = getCookie('voidllm_oidc_token')
    const expiresAt = getCookie('voidllm_oidc_expires_at')
    if (token) {
      storeSession(token, expiresAt ?? undefined)
      document.cookie = 'voidllm_oidc_token=; path=/auth/callback; max-age=0'
      document.cookie = 'voidllm_oidc_expires_at=; path=/auth/callback; max-age=0'
      window.location.href = '/'
    } else {
      window.location.href = '/login?error=sso_error'
    }
  }, [])

  return (
    <div className="min-h-screen flex items-center justify-center bg-bg-primary">
      <div className="text-center">
        <div className="inline-block h-6 w-6 animate-spin rounded-full border-2 border-accent border-t-transparent mb-4" />
        <p className="text-sm text-text-tertiary">Authenticating...</p>
      </div>
    </div>
  )
}
