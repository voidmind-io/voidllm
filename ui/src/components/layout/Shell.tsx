import { Outlet } from 'react-router-dom'
import { Sidebar } from './Sidebar'

export function Shell() {
  return (
    <div className="min-h-screen bg-bg-primary">
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:p-3 focus:bg-accent focus:text-white focus:rounded-md focus:m-2"
      >
        Skip to content
      </a>
      <Sidebar />
      <main id="main-content" className="ml-[260px] max-w-[calc(100%-260px)] p-8">
        <Outlet />
      </main>
    </div>
  )
}
