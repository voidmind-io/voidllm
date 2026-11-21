package admin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gofiber/fiber/v3"
	apihealth "github.com/voidmind-io/voidllm/internal/api/health"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/license"
)

const (
	// trialConsentTextVersion identifies the consent wording shown in the UI
	// when the trial was requested.
	trialConsentTextVersion = "2026-11-20"

	// trialSource tells the license server the request came from the product.
	trialSource = "product"

	trialRequestTimeout = 10 * time.Second
	maxTrialEmailLen    = 254
	maxTrialCompanyLen  = 200

	trialRejectedMessage    = "The license server rejected the request. Check the email address."
	trialUnavailableMessage = "Could not reach the license server. You can request a trial at https://voidllm.ai/trial"
	trialRateLimitMessage   = "Too many trial requests. Please try again later."
)

// defaultTrialClient never follows redirects and bounds every request.
var defaultTrialClient = &http.Client{
	Timeout: trialRequestTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// trialRequest is the JSON body accepted by RequestTrial.
type trialRequest struct {
	// Email is the address the trial key is sent to.
	Email string `json:"email"`
	// Company is an optional company name.
	Company string `json:"company"`
	// ConsentTrial must be true: consent to the trial processing terms.
	ConsentTrial bool `json:"consent_trial"`
	// ConsentMarketing is optional consent to product emails.
	ConsentMarketing bool `json:"consent_marketing"`
}

// trialUpstreamRequest is the payload relayed to the license server.
type trialUpstreamRequest struct {
	Email              string `json:"email"`
	Company            string `json:"company"`
	ConsentTrial       bool   `json:"consent_trial"`
	ConsentMarketing   bool   `json:"consent_marketing"`
	ConsentTextVersion string `json:"consent_text_version"`
	Source             string `json:"source"`
	InstanceID         string `json:"instance_id"`
	ProductVersion     string `json:"product_version"`
}

// trialResponse is the JSON body returned on an accepted trial request.
type trialResponse struct {
	// Status is always "check_inbox" on success.
	Status string `json:"status"`
}

// RequestTrial handles POST /api/v1/license/trial.
//
// It relays a trial request to the license server, which emails the key to
// the given address. Nothing is sent until an administrator submits this form.
//
//	@Summary		Request a trial license
//	@Description	Sends a trial request to the VoidLLM license server. The key is delivered by email.
//	@Tags			license
//	@Accept			json
//	@Produce		json
//	@Param			body	body		trialRequest	true	"Trial request"
//	@Success		202		{object}	trialResponse
//	@Failure		400		{object}	swaggerErrorResponse
//	@Failure		429		{object}	swaggerErrorResponse
//	@Failure		502		{object}	swaggerErrorResponse
//	@Router			/license/trial [post]
//	@Security		BearerAuth
func (h *Handler) RequestTrial(c fiber.Ctx) error {
	var req trialRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	email := strings.TrimSpace(req.Email)
	company := strings.TrimSpace(req.Company)
	if !validTrialEmail(email) {
		return apierror.BadRequest(c, "a valid email is required")
	}
	if utf16Len(company) > maxTrialCompanyLen {
		return apierror.BadRequest(c, "company must be at most 200 characters")
	}
	if !req.ConsentTrial {
		return apierror.BadRequest(c, "consent_trial must be true")
	}

	ctx := c.Context()
	instanceID, err := license.GetOrCreateInstanceID(ctx, h.DB)
	if err != nil {
		h.Log.LogAttrs(ctx, slog.LevelError, "trial request: instance id unavailable",
			slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to resolve instance id")
	}

	status, err := h.postTrial(ctx, trialUpstreamRequest{
		Email:              email,
		Company:            company,
		ConsentTrial:       req.ConsentTrial,
		ConsentMarketing:   req.ConsentMarketing,
		ConsentTextVersion: trialConsentTextVersion,
		Source:             trialSource,
		InstanceID:         instanceID,
		ProductVersion:     apihealth.Version,
	})
	switch {
	case err != nil:
		h.Log.LogAttrs(ctx, slog.LevelWarn, "trial request failed", slog.String("category", "network"))
		return apierror.Send(c, fiber.StatusBadGateway, "trial_unavailable", trialUnavailableMessage)
	case status == http.StatusAccepted:
		h.Log.LogAttrs(ctx, slog.LevelInfo, "trial requested")
		return c.Status(fiber.StatusAccepted).JSON(trialResponse{Status: "check_inbox"})
	case status == http.StatusBadRequest:
		h.Log.LogAttrs(ctx, slog.LevelInfo, "trial request failed", slog.String("category", "rejected"))
		return apierror.Send(c, fiber.StatusBadRequest, "trial_rejected", trialRejectedMessage)
	case status == http.StatusTooManyRequests:
		h.Log.LogAttrs(ctx, slog.LevelInfo, "trial request failed", slog.String("category", "rate_limited"))
		return apierror.Send(c, fiber.StatusTooManyRequests, "rate_limited", trialRateLimitMessage)
	default:
		h.Log.LogAttrs(ctx, slog.LevelWarn, "trial request failed",
			slog.String("category", "upstream_status"), slog.Int("status", status))
		return apierror.Send(c, fiber.StatusBadGateway, "trial_unavailable", trialUnavailableMessage)
	}
}

// postTrial sends the payload to the license server and returns the HTTP
// status code. The error never contains request data.
func (h *Handler) postTrial(ctx context.Context, payload trialUpstreamRequest) (int, error) {
	body, err := jsonx.Marshal(payload)
	if err != nil {
		return 0, err
	}
	base := h.trialBaseURL
	if base == "" {
		base = license.DefaultServerURL
	}
	client := h.trialClient
	if client == nil {
		client = defaultTrialClient
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+license.TrialRequestPath, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "voidllm/"+apihealth.Version)

	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused; the status is
	// already known, so a read failure here is irrelevant.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// utf16Len returns the length of s in UTF-16 code units, the unit the license
// server counts, so both sides agree on the limits for non-ASCII input.
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// validTrialEmail reports whether s is a bare RFC 5322 address of acceptable length.
func validTrialEmail(s string) bool {
	if s == "" || utf16Len(s) > maxTrialEmailLen {
		return false
	}
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s
}
