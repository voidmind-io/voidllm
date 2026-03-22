import { useState, useEffect } from 'react'
import type { FormEvent } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Input } from '../../components/ui/Input'
import { Button } from '../../components/ui/Button'
import { Banner } from '../../components/ui/Banner'
import { LOCAL_STORAGE_KEY } from '../../lib/constants'
import type { MeResponse } from '../../hooks/useMe'

interface AuthProviders {
  local: boolean
  oidc: boolean
}

const SSO_ERROR_MESSAGES: Record<string, string> = {
  not_provisioned: 'Your account has not been provisioned. Please contact your administrator.',
  domain_not_allowed: 'Your email domain is not authorized for SSO login.',
  sso_error: 'SSO authentication failed. Please try again.',
}

export default function LoginPage() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [searchParams] = useSearchParams()

  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [providers, setProviders] = useState<AuthProviders | null>(null)

  // Surface any SSO error from the URL query string
  const ssoErrorParam = searchParams.get('error')
  const ssoError =
    ssoErrorParam !== null ? (SSO_ERROR_MESSAGES[ssoErrorParam] ?? null) : null

  useEffect(() => {
    fetch('/api/v1/auth/providers')
      .then((res) => {
        if (!res.ok) return
        return res.json() as Promise<AuthProviders>
      })
      .then((data) => {
        if (data !== undefined) setProviders(data)
      })
      .catch(() => {
        // Non-critical — if the endpoint fails we simply don't show the SSO button
      })
  }, [])

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    setError(null)
    setLoading(true)

    try {
      const res = await fetch('/api/v1/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, password }),
      })

      if (!res.ok) {
        const body = await res.json().catch(() => ({ error: { message: res.statusText } }))
        setError((body as { error?: { message?: string } })?.error?.message ?? 'Login failed')
        return
      }

      const data = (await res.json()) as { token: string; expires_at: string; user: MeResponse }
      localStorage.setItem(LOCAL_STORAGE_KEY, data.token)
      queryClient.setQueryData(['me'], data.user)
      navigate('/')
    } catch {
      setError('Unable to reach the server. Check your connection.')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-bg-primary px-4">
      <div className="w-full max-w-sm bg-bg-secondary border border-white/5 rounded-xl p-8">
        <div className="mb-8 text-center">
          <h1 className="text-3xl font-bold gradient-text">VoidLLM</h1>
          <p className="mt-2 text-sm text-text-tertiary">Sign in to your workspace</p>
        </div>

        {ssoError !== null && (
          <Banner variant="error" title={ssoError} className="mb-5" />
        )}

        <form onSubmit={(e) => void handleSubmit(e)} className="space-y-5">
          <Input
            label="Email"
            type="email"
            autoComplete="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="you@example.com"
          />

          <Input
            label="Password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="••••••••"
          />

          {error !== null && <Banner variant="error" title={error} />}

          <Button type="submit" loading={loading} fullWidth size="lg">
            Sign in
          </Button>
        </form>

        {providers?.oidc === true && (
          <>
            <div className="my-6 flex items-center gap-3">
              <div className="flex-1 h-px bg-white/10" />
              <span className="text-xs text-text-tertiary">or</span>
              <div className="flex-1 h-px bg-white/10" />
            </div>

            <a href="/api/v1/auth/oidc/login" className="block w-full">
              <Button variant="secondary" fullWidth size="lg" type="button">
                Sign in with SSO
              </Button>
            </a>
          </>
        )}
      </div>
    </div>
  )
}
