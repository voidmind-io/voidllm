package app

import (
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"

	"github.com/gofiber/fiber/v3"

	uiPkg "github.com/voidmind-io/voidllm/ui"
)

// registerSPAHandler mounts the embedded Vite SPA as a catch-all route on app.
// Static assets under assets/ are served with long-lived immutable cache headers
// (Vite uses content-hashed filenames). All other paths fall back to index.html
// so that the React router handles client-side navigation.
//
// This must be called AFTER all API and proxy routes are registered so it does
// not shadow any existing paths.
func registerSPAHandler(fiberApp *fiber.App, log *slog.Logger) {
	uiSub, err := fs.Sub(uiPkg.DistFS, "dist")
	if err != nil {
		log.Error("failed to sub embedded UI fs", slog.String("error", err.Error()))
		return
	}

	indexFile, err := uiSub.Open("index.html")
	if err != nil {
		log.Error("embedded UI has no index.html", slog.String("error", err.Error()))
		return
	}
	indexData, err := io.ReadAll(indexFile)
	_ = indexFile.Close() // embedded fs; close error is irrelevant
	if err != nil {
		log.Error("failed to read embedded index.html", slog.String("error", err.Error()))
		return
	}

	fiberApp.Get("/*", func(c fiber.Ctx) error {
		reqPath := c.Path()

		// Root always serves index.html.
		if reqPath != "/" {
			cleanPath := strings.TrimPrefix(reqPath, "/")
			f, openErr := uiSub.Open(cleanPath)
			if openErr == nil {
				stat, statErr := f.Stat()
				if statErr == nil && !stat.IsDir() {
					data, readErr := io.ReadAll(f)
					_ = f.Close() // embedded fs; close error is irrelevant
					if readErr != nil {
						return fiber.ErrInternalServerError
					}
					ext := filepath.Ext(cleanPath)
					ct := mime.TypeByExtension(ext)
					if ct == "" {
						ct = "application/octet-stream"
					}
					c.Set("Content-Type", ct)
					c.Set("X-Content-Type-Options", "nosniff")
					// Vite hashes asset filenames — safe to cache permanently.
					if strings.HasPrefix(cleanPath, "assets/") {
						c.Set("Cache-Control", "public, max-age=31536000, immutable")
					}
					return c.Send(data)
				}
				_ = f.Close() // embedded fs; close error is irrelevant
			}
		}

		// SPA fallback: let the React router handle the path.
		// embed.FS rejects paths containing ".." — no traversal risk.
		c.Set("Content-Type", "text/html; charset=utf-8")
		c.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Set("X-Content-Type-Options", "nosniff")
		return c.Send(indexData)
	})
}
