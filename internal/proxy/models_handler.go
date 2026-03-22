package proxy

import (
	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
)

// modelEntry is the OpenAI-compatible representation of a single model as
// returned by GET /v1/models.
type modelEntry struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	OwnedBy string   `json:"owned_by"`
	Aliases []string `json:"aliases,omitempty"`
}

// modelsResponse is the top-level envelope for the GET /v1/models response,
// matching the OpenAI models list format.
type modelsResponse struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// ModelsHandler handles GET /v1/models and returns the models the caller is
// permitted to access in an OpenAI-compatible list format.
// Sensitive fields (APIKey, BaseURL) are never included in the response.
// When AccessCache is set, each model is filtered through the in-memory
// org/team/key access control allowlists. Models that fail the access check
// are excluded (fail-closed).
func (p *ProxyHandler) ModelsHandler(c fiber.Ctx) error {
	allModels := p.Registry.ListInfo()
	keyInfo := auth.KeyInfoFromCtx(c)

	var accessible []ModelInfo
	if p.AccessCache == nil || keyInfo == nil {
		// No access-control available: return everything.
		accessible = allModels
	} else {
		accessible = make([]ModelInfo, 0, len(allModels))
		for _, m := range allModels {
			if p.AccessCache.Check(keyInfo.OrgID, keyInfo.TeamID, keyInfo.ID, m.Name) {
				accessible = append(accessible, m)
			}
		}
	}

	data := make([]modelEntry, len(accessible))
	for i, m := range accessible {
		data[i] = modelEntry{
			ID:      m.Name,
			Object:  "model",
			Created: 0,
			OwnedBy: "voidllm",
			Aliases: m.Aliases,
		}
	}

	return c.JSON(modelsResponse{Object: "list", Data: data})
}
