package admin

import (
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/money"
)

const (
	fieldDailyCostLimit   = "daily_cost_limit"
	fieldMonthlyCostLimit = "monthly_cost_limit"
)

// parseCostLimit parses one cost limit request field. An absent field or JSON
// null yields nil (unchanged on update, unlimited on create). The error names
// the field.
func parseCostLimit(field string, raw json.RawMessage) (*money.Micros, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m money.Micros
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New(field + " must be a non-negative decimal amount with at most 6 decimal places, for example \"25.00\"")
	}
	return &m, nil
}

// costLimitChange is a parsed pair of cost limit request fields. A nil field
// was not sent: unchanged on update, unlimited on create.
type costLimitChange struct {
	Daily   *money.Micros
	Monthly *money.Micros
}

// dailyValue returns the daily limit to store on create, zero when not sent.
func (ch costLimitChange) dailyValue() money.Micros {
	if ch.Daily == nil {
		return 0
	}
	return *ch.Daily
}

// monthlyValue returns the monthly limit to store on create, zero when not sent.
func (ch costLimitChange) monthlyValue() money.Micros {
	if ch.Monthly == nil {
		return 0
	}
	return *ch.Monthly
}

// differsFrom reports whether a sent field differs from the stored value.
func (ch costLimitChange) differsFrom(daily, monthly money.Micros) bool {
	return (ch.Daily != nil && *ch.Daily != daily) ||
		(ch.Monthly != nil && *ch.Monthly != monthly)
}

// setsNonZero reports whether any sent field is non-zero.
func (ch costLimitChange) setsNonZero() bool {
	return (ch.Daily != nil && *ch.Daily != 0) ||
		(ch.Monthly != nil && *ch.Monthly != 0)
}

// parseCostLimits parses the daily and monthly cost limit request fields. It
// sends a 400 naming the offending field and reports sent=true when either is
// invalid.
func parseCostLimits(c fiber.Ctx, daily, monthly json.RawMessage) (costLimitChange, bool, error) {
	d, err := parseCostLimit(fieldDailyCostLimit, daily)
	if err != nil {
		return costLimitChange{}, true, apierror.BadRequest(c, err.Error())
	}
	m, err := parseCostLimit(fieldMonthlyCostLimit, monthly)
	if err != nil {
		return costLimitChange{}, true, apierror.BadRequest(c, err.Error())
	}
	return costLimitChange{Daily: d, Monthly: m}, false, nil
}

// rejectUnlicensedCostLimits enforces the license gate on writing cost limits.
// Setting a non-zero limit requires license.CostBudgetsEnabled; zero is always
// allowed, and so is re-sending the value that is already stored, so stored
// limits survive a lapsed license and clients may echo them back unchanged.
// current loads the stored values and is only called when the license lacks
// the feature and a non-zero value was sent; nil means there is no stored
// record (create), where every stored value counts as zero. notFound is the
// 404 message used when current reports db.ErrNotFound. It sends the response
// and reports sent=true when the write must be rejected.
func (h *Handler) rejectUnlicensedCostLimits(c fiber.Ctx, ch costLimitChange, current func() (money.Micros, money.Micros, error), notFound string) (bool, error) {
	if !ch.setsNonZero() || license.CostBudgetsEnabled(h.License.Load()) {
		return false, nil
	}
	var curDaily, curMonthly money.Micros
	if current != nil {
		var err error
		curDaily, curMonthly, err = current()
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return true, apierror.NotFound(c, notFound)
			}
			h.Log.ErrorContext(c.Context(), "load stored cost limits", slog.String("error", err.Error()))
			return true, apierror.InternalError(c, "failed to check cost limits")
		}
	}
	if (ch.Daily != nil && *ch.Daily != 0 && *ch.Daily != curDaily) ||
		(ch.Monthly != nil && *ch.Monthly != 0 && *ch.Monthly != curMonthly) {
		return true, apierror.Send(c, fiber.StatusForbidden, "feature_unavailable",
			"cost limits require a Pro license")
	}
	return false, nil
}
