import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import { Markdown } from './Markdown'

describe('Markdown', () => {
  describe('Headings', () => {
    it('renders ## as an h2 with the heading text', () => {
      render(<Markdown>{'## Features'}</Markdown>)
      const heading = screen.getByRole('heading', { level: 2, name: 'Features' })
      expect(heading).toBeInTheDocument()
      expect(heading.tagName).toBe('H2')
    })

    it('renders # as an h1', () => {
      render(<Markdown>{'# Title'}</Markdown>)
      expect(screen.getByRole('heading', { level: 1, name: 'Title' })).toBeInTheDocument()
    })

    it('renders ### as an h3', () => {
      render(<Markdown>{'### Sub-section'}</Markdown>)
      expect(screen.getByRole('heading', { level: 3, name: 'Sub-section' })).toBeInTheDocument()
    })
  })

  describe('Lists', () => {
    it('renders an unordered list with li items', () => {
      render(<Markdown>{'- a\n- b'}</Markdown>)
      const items = screen.getAllByRole('listitem')
      expect(items).toHaveLength(2)
      expect(items[0].textContent).toBe('a')
      expect(items[1].textContent).toBe('b')
    })

    it('unordered list items are inside a ul element', () => {
      render(<Markdown>{'- first\n- second'}</Markdown>)
      const list = screen.getByRole('list')
      expect(list.tagName).toBe('UL')
      expect(list.querySelectorAll('li')).toHaveLength(2)
    })

    it('renders an ordered list', () => {
      render(<Markdown>{'1. one\n2. two'}</Markdown>)
      const items = screen.getAllByRole('listitem')
      expect(items).toHaveLength(2)
      expect(items[0].textContent).toBe('one')
      expect(items[1].textContent).toBe('two')
    })
  })

  describe('Inline formatting', () => {
    it('renders **x** as a strong element', () => {
      render(<Markdown>{'**bold text**'}</Markdown>)
      const el = document.querySelector('strong')
      expect(el).not.toBeNull()
      expect(el?.textContent).toBe('bold text')
    })

    it('renders `code` as an inline code element', () => {
      render(<Markdown>{'Use `console.log` here'}</Markdown>)
      const el = document.querySelector('code')
      expect(el).not.toBeNull()
      expect(el?.textContent).toBe('console.log')
    })

    it('renders *x* as an em element', () => {
      render(<Markdown>{'*italic*'}</Markdown>)
      const el = document.querySelector('em')
      expect(el).not.toBeNull()
      expect(el?.textContent).toBe('italic')
    })
  })

  describe('Links', () => {
    it('renders [text](url) as an anchor with the correct href', () => {
      render(<Markdown>{'[VoidLLM](https://example.com)'}</Markdown>)
      const link = screen.getByRole('link', { name: 'VoidLLM' })
      expect(link).toBeInTheDocument()
      expect(link).toHaveAttribute('href', 'https://example.com')
    })

    it('link has target="_blank"', () => {
      render(<Markdown>{'[text](https://example.com)'}</Markdown>)
      expect(screen.getByRole('link')).toHaveAttribute('target', '_blank')
    })

    it('link rel contains noopener', () => {
      render(<Markdown>{'[text](https://example.com)'}</Markdown>)
      const rel = screen.getByRole('link').getAttribute('rel') ?? ''
      expect(rel).toContain('noopener')
    })

    it('link rel contains noreferrer', () => {
      render(<Markdown>{'[text](https://example.com)'}</Markdown>)
      const rel = screen.getByRole('link').getAttribute('rel') ?? ''
      expect(rel).toContain('noreferrer')
    })
  })

  describe('GFM extensions', () => {
    it('renders a fenced code block inside a pre element', () => {
      render(<Markdown>{'```\nconst x = 1\n```'}</Markdown>)
      const pre = document.querySelector('pre')
      expect(pre).not.toBeNull()
      expect(pre?.textContent).toContain('const x = 1')
    })

    it('fenced code block contains a code child', () => {
      render(<Markdown>{'```\nhello\n```'}</Markdown>)
      const pre = document.querySelector('pre')
      expect(pre?.querySelector('code')).not.toBeNull()
    })

    it('renders ~~x~~ as a del element (GFM strikethrough)', () => {
      render(<Markdown>{'~~deprecated~~'}</Markdown>)
      const del = document.querySelector('del')
      expect(del).not.toBeNull()
      expect(del?.textContent).toBe('deprecated')
    })

    it('renders a GFM table as a table element', () => {
      render(
        <Markdown>
          {'| Name | Value |\n| --- | --- |\n| foo | bar |'}
        </Markdown>
      )
      expect(document.querySelector('table')).not.toBeNull()
      expect(document.querySelector('th')?.textContent?.trim()).toBe('Name')
      expect(document.querySelector('td')?.textContent?.trim()).toBe('foo')
    })

    it('renders ![alt](url) as an img with correct src and alt', () => {
      render(<Markdown>{'![logo](https://example.com/logo.png)'}</Markdown>)
      const img = document.querySelector('img')
      expect(img).not.toBeNull()
      expect(img).toHaveAttribute('src', 'https://example.com/logo.png')
      expect(img).toHaveAttribute('alt', 'logo')
    })
  })

  describe('XSS safety — raw HTML is not executed', () => {
    it('does not create a script element from raw <script> input', () => {
      render(<Markdown>{'<script>alert(1)</script>'}</Markdown>)
      expect(document.querySelector('script')).toBeNull()
    })

    it('does not create an img element from <img onerror> input', () => {
      render(<Markdown>{'<img src=x onerror="alert(1)">'}</Markdown>)
      // react-markdown without rehype-raw escapes the tag to text, so no img in the DOM
      expect(document.querySelector('img')).toBeNull()
    })

    it('<script> content appears as escaped text, not as a live element', () => {
      render(<Markdown>{'<script>alert(1)</script>'}</Markdown>)
      // The raw string is rendered as visible text, not executed
      const text = document.body.textContent ?? ''
      expect(text).toContain('<script>')
    })

    it('does not create an iframe from raw <iframe> input', () => {
      render(<Markdown>{'<iframe src="https://evil.example"></iframe>'}</Markdown>)
      expect(document.querySelector('iframe')).toBeNull()
    })

    it('does not create a style element from raw <style> input', () => {
      render(<Markdown>{'<style>body{display:none}</style>'}</Markdown>)
      // Only the document head can have a pre-existing style element — ensure none were injected
      const injected = document.querySelectorAll('style')
      // Any style elements that exist are from jsdom/test infrastructure, not from the component.
      // The component itself should not create a new <style> inside the rendered output.
      // Check that the rendered markdown did not introduce a live <style> element as a child
      // of the markdown output (react-markdown wraps output in the document body during test)
      for (const s of Array.from(injected)) {
        expect(s.textContent).not.toContain('body{display:none}')
      }
    })
  })

  describe('Edge cases', () => {
    it('renders an empty string without crashing', () => {
      expect(() => render(<Markdown>{''}</Markdown>)).not.toThrow()
    })

    it('renders plain text without any block wrapper element pollution', () => {
      render(<Markdown>{'Hello world'}</Markdown>)
      expect(screen.getByText('Hello world')).toBeInTheDocument()
    })

    it('renders a blockquote for > quoted text', () => {
      render(<Markdown>{'> a note'}</Markdown>)
      const bq = document.querySelector('blockquote')
      expect(bq).not.toBeNull()
      expect(bq?.textContent?.trim()).toBe('a note')
    })

    it('renders a horizontal rule for ---', () => {
      render(<Markdown>{'before\n\n---\n\nafter'}</Markdown>)
      expect(document.querySelector('hr')).not.toBeNull()
    })
  })
})
