/** Sanitize a CSV cell to prevent formula injection and handle special characters. */
function sanitizeCSVCell(value: unknown): string {
  let s = String(value ?? '')
  if (['=', '+', '-', '@', '\t', '\r'].some((c) => s.startsWith(c))) {
    s = "'" + s
  }
  if (s.includes(',') || s.includes('"') || s.includes('\n')) {
    return '"' + s.replace(/"/g, '""') + '"'
  }
  return s
}

/** Trigger a file download in the browser. */
function downloadFile(content: string, filename: string, mimeType: string) {
  const blob = new Blob([content], { type: mimeType })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}

/** Export data as CSV or JSON. */
export function exportData(
  data: Record<string, unknown>[],
  headers: { key: string; label: string }[],
  filenamePrefix: string,
  format: 'csv' | 'json',
) {
  const dateStr = new Date().toISOString().slice(0, 10)

  if (format === 'json') {
    const jsonData = data.map((row) => {
      const obj: Record<string, unknown> = {}
      for (const h of headers) {
        obj[h.key] = row[h.key]
      }
      return obj
    })
    downloadFile(
      JSON.stringify(jsonData, null, 2),
      `${filenamePrefix}-${dateStr}.json`,
      'application/json',
    )
    return
  }

  // CSV
  const headerRow = headers.map((h) => h.label)
  const rows = data.map((row) => headers.map((h) => row[h.key]))
  const csv = [headerRow, ...rows]
    .map((r) => r.map(sanitizeCSVCell).join(','))
    .join('\n')
  downloadFile(csv, `${filenamePrefix}-${dateStr}.csv`, 'text/csv')
}
