import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { Components } from 'react-markdown'

/**
 * Renders a markdown string into styled HTML matching the VoidLLM dark UI.
 * Raw HTML passthrough is intentionally disabled (no rehype-raw) to keep
 * semi-trusted content (e.g. GitHub release notes) XSS-safe.
 */
export function Markdown({ children }: { children: string }) {
  return (
    <ReactMarkdown remarkPlugins={[remarkGfm]} components={markdownComponents}>
      {children}
    </ReactMarkdown>
  )
}

const markdownComponents: Components = {
  h1: ({ children }) => (
    <h1 className="text-base font-bold text-text-primary mt-4 mb-2 first:mt-0">
      {children}
    </h1>
  ),
  h2: ({ children }) => (
    <h2 className="text-sm font-semibold text-text-primary mt-4 mb-2 first:mt-0">
      {children}
    </h2>
  ),
  h3: ({ children }) => (
    <h3 className="text-sm font-semibold text-text-secondary mt-3 mb-1 first:mt-0">
      {children}
    </h3>
  ),
  p: ({ children }) => (
    <p className="text-sm text-text-secondary mb-2 last:mb-0 leading-relaxed">
      {children}
    </p>
  ),
  ul: ({ children }) => (
    <ul className="list-disc list-outside pl-4 mb-2 space-y-0.5 text-sm text-text-secondary">
      {children}
    </ul>
  ),
  ol: ({ children }) => (
    <ol className="list-decimal list-outside pl-4 mb-2 space-y-0.5 text-sm text-text-secondary">
      {children}
    </ol>
  ),
  li: ({ children }) => (
    <li className="leading-relaxed">
      {children}
    </li>
  ),
  a: ({ href, children }) => (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="text-accent hover:underline underline-offset-2"
    >
      {children}
    </a>
  ),
  code: ({ children, className }) => {
    const isBlock =
      (typeof className === 'string' && className.startsWith('language-')) ||
      String(children).includes('\n')
    if (isBlock) {
      return (
        <code className="block font-mono text-xs text-text-secondary leading-relaxed">
          {children}
        </code>
      )
    }
    return (
      <code className="font-mono text-xs text-text-primary bg-bg-tertiary rounded px-1 py-0.5 border border-border">
        {children}
      </code>
    )
  },
  pre: ({ children }) => (
    <pre className="bg-bg-tertiary border border-border rounded-md p-3 mb-2 overflow-x-auto text-xs font-mono text-text-secondary">
      {children}
    </pre>
  ),
  strong: ({ children }) => (
    <strong className="font-semibold text-text-primary">
      {children}
    </strong>
  ),
  em: ({ children }) => (
    <em className="italic text-text-secondary">
      {children}
    </em>
  ),
  blockquote: ({ children }) => (
    <blockquote className="border-l-2 border-accent/40 pl-3 mb-2 text-text-tertiary italic">
      {children}
    </blockquote>
  ),
  hr: () => (
    <hr className="border-0 border-t border-border my-3" />
  ),
  del: ({ children }) => (
    <del className="line-through text-text-tertiary">
      {children}
    </del>
  ),
  img: ({ src, alt }) => (
    <img
      src={src}
      alt={alt}
      className="max-w-full h-auto rounded-md border border-border my-2"
    />
  ),
  table: ({ children }) => (
    <div className="overflow-x-auto mb-2">
      <table className="w-full text-sm text-text-secondary border-collapse">
        {children}
      </table>
    </div>
  ),
  thead: ({ children }) => (
    <thead className="border-b border-border">
      {children}
    </thead>
  ),
  tbody: ({ children }) => (
    <tbody>
      {children}
    </tbody>
  ),
  tr: ({ children }) => (
    <tr className="border-b border-border/50">
      {children}
    </tr>
  ),
  th: ({ children }) => (
    <th className="text-left text-text-primary font-semibold px-2 py-1.5 text-xs">
      {children}
    </th>
  ),
  td: ({ children }) => (
    <td className="px-2 py-1.5 text-xs">
      {children}
    </td>
  ),
  input: ({ checked, disabled }) => (
    <input
      type="checkbox"
      checked={checked}
      disabled={disabled}
      readOnly
      className="mr-1 align-middle accent-accent"
    />
  ),
}
