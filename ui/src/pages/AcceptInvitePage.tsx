import { useState, useEffect, type FormEvent } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { Input } from '../components/ui/Input'
import { Button } from '../components/ui/Button'

interface InvitePeek {
  email: string
  org_name: string
  role: string
  expires_at: string
}

type PageState = 'loading' | 'invalid' | 'expired' | 'form' | 'success'

export default function AcceptInvitePage() {
  const { token } = useParams<{ token: string }>()
  const navigate = useNavigate()

  const [pageState, setPageState] = useState<PageState>('loading')
  const [invite, setInvite] = useState<InvitePeek | null>(null)

  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')

  const [displayNameError, setDisplayNameError] = useState<string | undefined>()
  const [passwordError, setPasswordError] = useState<string | undefined>()
  const [confirmPasswordError, setConfirmPasswordError] = useState<string | undefined>()
  const [submitError, setSubmitError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!token) {
      setPageState('invalid')
      return
    }

    fetch(`/api/v1/invites/peek?token=${encodeURIComponent(token)}`)
      .then((res) => {
        if (!res.ok) {
          throw new Error(res.status === 410 ? 'expired' : 'invalid')
        }
        return res.json() as Promise<InvitePeek>
      })
      .then((data) => {
        setInvite(data)
        setPageState('form')
      })
      .catch((err: unknown) => {
        const msg = err instanceof Error ? err.message : 'invalid'
        setPageState(msg === 'expired' ? 'expired' : 'invalid')
      })
  }, [token])

  useEffect(() => {
    if (pageState !== 'success') return
    const timer = setTimeout(() => {
      void navigate('/login')
    }, 2000)
    return () => clearTimeout(timer)
  }, [pageState, navigate])

  function validate(): boolean {
    let valid = true

    if (password.length < 8) {
      setPasswordError('Password must be at least 8 characters')
      valid = false
    } else {
      setPasswordError(undefined)
    }

    if (confirmPassword !== password) {
      setConfirmPasswordError('Passwords do not match')
      valid = false
    } else {
      setConfirmPasswordError(undefined)
    }

    if (!displayName.trim()) {
      setDisplayNameError('Display name is required')
      valid = false
    } else {
      setDisplayNameError(undefined)
    }

    return valid
  }

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (!validate()) return

    setSubmitError(null)
    setSubmitting(true)

    try {
      const res = await fetch('/api/v1/invites/redeem', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          token,
          password,
          display_name: displayName.trim(),
        }),
      })

      if (!res.ok) {
        const body = await res.json().catch(() => ({ error: { message: res.statusText } }))
        const msg =
          (body as { error?: { message?: string } })?.error?.message ?? 'Failed to accept invite'
        setSubmitError(msg)
        return
      }

      setPageState('success')
    } catch {
      setSubmitError('Unable to reach the server. Check your connection.')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-bg-primary px-4">
      <div className="w-full max-w-sm bg-bg-secondary border border-white/5 rounded-xl p-8">
        <div className="mb-8 text-center">
          <h1 className="text-3xl font-bold gradient-text">VoidLLM</h1>
        </div>

        {pageState === 'loading' && (
          <div className="space-y-3">
            <div className="h-4 rounded bg-bg-tertiary animate-pulse" />
            <div className="h-4 w-2/3 rounded bg-bg-tertiary animate-pulse" />
            <div className="mt-6 h-10 rounded bg-bg-tertiary animate-pulse" />
          </div>
        )}

        {(pageState === 'invalid' || pageState === 'expired') && (
          <div className="text-center space-y-4">
            <div className="rounded-lg bg-error/10 border border-error/20 px-4 py-3">
              <p className="text-sm font-medium text-error">
                {pageState === 'expired'
                  ? 'This invite link has expired.'
                  : 'This invite link is invalid or has already been used.'}
              </p>
            </div>
            <p className="text-sm text-text-tertiary">
              Ask your organization admin for a new invite.
            </p>
            <Link
              to="/login"
              className="inline-block text-sm text-accent hover:text-accent/80 transition-colors"
            >
              Go to sign in
            </Link>
          </div>
        )}

        {pageState === 'success' && (
          <div className="text-center space-y-4">
            <div className="rounded-lg bg-success/10 border border-success/20 px-4 py-3">
              <p className="text-sm font-medium text-success">
                Account created successfully!
              </p>
            </div>
            <p className="text-sm text-text-tertiary">
              Redirecting you to sign in...
            </p>
          </div>
        )}

        {pageState === 'form' && invite !== null && (
          <>
            <div className="mb-6">
              <p className="text-sm text-text-secondary text-center">
                You've been invited to join
              </p>
              <p className="text-lg font-semibold text-text-primary text-center mt-1">
                {invite.org_name}
              </p>
            </div>

            <form onSubmit={(e) => void handleSubmit(e)} className="space-y-5" noValidate>
              <div>
                <label className="block text-sm font-medium text-text-secondary mb-1.5">
                  Email
                </label>
                <div className="block w-full rounded-md bg-bg-tertiary border border-border px-3 py-2 text-sm text-text-secondary select-none">
                  {invite.email}
                </div>
              </div>

              <Input
                label="Display Name"
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                placeholder="Jane Smith"
                error={displayNameError}
                disabled={submitting}
                autoComplete="name"
              />

              <Input
                label="Password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="Min. 8 characters"
                error={passwordError}
                disabled={submitting}
                autoComplete="new-password"
              />

              <Input
                label="Confirm Password"
                type="password"
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
                placeholder="Re-enter your password"
                error={confirmPasswordError}
                disabled={submitting}
                autoComplete="new-password"
              />

              {submitError !== null && (
                <div className="rounded-lg bg-error/10 border border-error/20 px-3 py-2">
                  <p className="text-xs text-error">{submitError}</p>
                </div>
              )}

              <Button type="submit" loading={submitting} fullWidth size="lg">
                Accept Invite
              </Button>
            </form>

            <p className="mt-6 text-center text-sm text-text-tertiary">
              Already have an account?{' '}
              <Link
                to="/login"
                className="text-accent hover:text-accent/80 transition-colors"
              >
                Sign in
              </Link>
            </p>
          </>
        )}
      </div>
    </div>
  )
}
