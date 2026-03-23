# VoidLLM UI

The admin dashboard for VoidLLM — a single-page application embedded in the Go binary via `embed.FS`.

## Tech Stack

- **React 18** + **TypeScript**
- **Vite 6** (dev server + build)
- **Tailwind CSS v4** (utility-first, no component library)
- **TanStack Query** (data fetching + cache)
- **React Router v7** (client-side routing)
- **Vitest** + **React Testing Library** (305 tests)

All components are custom — no shadcn, no MUI, no external component libraries.

## Development

```bash
# From the repo root:
cd ui
npm install
npm run dev
```

The dev server runs on `http://localhost:5173` and proxies API requests to `http://localhost:8080` (the Go backend). Start the backend separately with `air` or `go run ./cmd/voidllm`.

## Pages

| Page | Route | Description |
|---|---|---|
| Dashboard | `/` | Role-scoped stats, top models, recent usage |
| Keys | `/keys` | Create, rotate, revoke API keys |
| Teams | `/teams` | Team management + members |
| Models | `/models` | Model registry, aliases, access control |
| Usage | `/usage` | Usage analytics with time range + group by |
| Cost Reports | `/cost-reports` | Cost analysis + budget alerts |
| Organizations | `/orgs` | Multi-org management (enterprise) |
| Audit Log | `/audit-log` | Admin action history (enterprise) |
| SSO Config | `/orgs/:id/sso` | OIDC provider configuration (enterprise) |
| License | `/license` | Plan status, feature list, key activation |
| Settings | `/settings` | Org name, slug, rate limits |
| Profile | `/profile` | User profile, password change |
| Playground | `/playground` | Chat completions test interface |

## Custom Components

All UI components live in `src/components/ui/` with co-located tests:

`Badge` · `Banner` · `Button` · `CopyButton` · `Dialog` · `Input` · `KeyHint` · `PageHeader` · `Select` · `StatCard` · `Table` · `Tabs` · `Textarea` · `TimeAgo` · `Toggle` · `UpgradePrompt`

## Scripts

```bash
npm run dev        # Start dev server (port 5173)
npm run build      # Production build → dist/
npm run lint       # ESLint
npm run test       # Vitest (watch mode)
npm run test --run # Vitest (single run)
```

## Build + Embed

The production build (`npm run build`) outputs to `ui/dist/`. The Go binary embeds this directory via `ui/embed.go`:

```go
//go:embed dist/*
var Assets embed.FS
```

The Dockerfile handles this automatically — Stage 1 builds the UI, Stage 2 copies `dist/` into the Go build context.

## Color Palette

The UI uses a dark theme defined in `src/styles/globals.css`:

| Token | Value | Usage |
|---|---|---|
| `--bg-primary` | `#0a0a0f` | Page background |
| `--bg-secondary` | `#12121a` | Cards, panels |
| `--bg-tertiary` | `#1a1a24` | Inputs, hover states |
| `--text-primary` | `#e2e8f0` | Headings, body |
| `--text-secondary` | `#a1afc4` | Labels, descriptions |
| `--accent` | `#8b5cf6` | Buttons, links, focus rings |
| `--success` | `#22c55e` | Active states, confirmations |
| `--error` | `#ef4444` | Errors, destructive actions |

Fonts: **Inter** (UI) + **JetBrains Mono** (code), self-hosted via fontsource.
