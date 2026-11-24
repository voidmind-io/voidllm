// Package money provides an exact fixed-point amount type used for cost
// budgets. Amounts are stored as integer micro-units (1 unit = 1,000,000
// micros) so that parsing, storing and comparing limits never involves
// floating point.
package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Scale is the number of micro-units in one major currency unit.
const Scale = 1_000_000

// MaxUnits is the largest amount, in major units, that Parse accepts (one
// billion). It keeps MaxMicros (1e15) below 2^53 so sums of counters near the
// limit stay exact in float64 arithmetic such as Redis Lua.
const MaxUnits = 1_000_000_000

// MaxMicros is the largest amount, in micro-units, that Parse accepts.
const MaxMicros Micros = MaxUnits * Scale

const fractionDigits = 6

// ErrInvalid is returned (wrapped) when an amount cannot be parsed.
var ErrInvalid = errors.New("invalid amount")

// Micros is an amount of money in micro-units of the configured currency.
// Zero means unlimited when used as a limit.
type Micros int64

// Parse parses a non-negative decimal amount in major units such as "25",
// "0.5" or "25.000001". It accepts at most six fraction digits and amounts up
// to MaxUnits. Signs, exponents, thousands separators and surrounding spaces
// are rejected.
func Parse(s string) (Micros, error) {
	if s == "" {
		return 0, fmt.Errorf("parse amount %q: %w", s, ErrInvalid)
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && (!hasDot || frac == "") {
		return 0, fmt.Errorf("parse amount %q: %w", s, ErrInvalid)
	}
	if hasDot && frac == "" {
		return 0, fmt.Errorf("parse amount %q: %w", s, ErrInvalid)
	}
	if !allDigits(whole) || !allDigits(frac) {
		return 0, fmt.Errorf("parse amount %q: %w", s, ErrInvalid)
	}
	if len(frac) > fractionDigits {
		return 0, fmt.Errorf("parse amount %q: more than %d fraction digits: %w", s, fractionDigits, ErrInvalid)
	}
	whole = strings.TrimLeft(whole, "0")
	if len(whole) > 13 {
		return 0, fmt.Errorf("parse amount %q: too large: %w", s, ErrInvalid)
	}
	var units int64
	if whole != "" {
		u, err := strconv.ParseInt(whole, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse amount %q: %w", s, ErrInvalid)
		}
		units = u
	}
	if units > MaxUnits {
		return 0, fmt.Errorf("parse amount %q: too large: %w", s, ErrInvalid)
	}
	var fracMicros int64
	if frac != "" {
		padded := frac + strings.Repeat("0", fractionDigits-len(frac))
		f, err := strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse amount %q: %w", s, ErrInvalid)
		}
		fracMicros = f
	}
	total := Micros(units*Scale + fracMicros)
	if total > MaxMicros {
		return 0, fmt.Errorf("parse amount %q: too large: %w", s, ErrInvalid)
	}
	return total, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String formats the amount as a decimal in major units with at least two
// and at most six fraction digits, for example "25.00" or "0.000123".
func (m Micros) String() string {
	neg := m < 0
	v := uint64(m)
	if neg {
		v = -v
	}
	whole := v / Scale
	frac := strconv.FormatUint(v%Scale+Scale, 10)[1:]
	frac = strings.TrimRight(frac, "0")
	for len(frac) < 2 {
		frac += "0"
	}
	out := strconv.FormatUint(whole, 10) + "." + frac
	if neg {
		out = "-" + out
	}
	return out
}

// FromCost converts a cost in major currency units to micro-units, rounded to
// the nearest micro-unit (half away from zero). Values that are not positive
// (including NaN) convert to 0 and values above MaxMicros are clamped to
// MaxMicros, so every returned value round-trips through JSON. Live usage and
// seeded usage both go through it, so a request counts the same before and
// after a restart. SQL aggregates round per event with FLOOR(x+0.5) and may
// differ from this in the last micro for pathological values.
func FromCost(cost float64) Micros {
	if !(cost > 0) {
		return 0
	}
	micros := math.Round(cost * Scale)
	if micros >= float64(MaxMicros) {
		return MaxMicros
	}
	return Micros(micros)
}

// MarshalJSON encodes the amount as a JSON string holding its decimal form.
func (m Micros) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON decodes an amount from a JSON string or a JSON number. A
// number is parsed from its literal text, never through a float.
func (m *Micros) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("decode amount: %w", ErrInvalid)
	}
	var text string
	if data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return fmt.Errorf("decode amount: %w", ErrInvalid)
		}
	} else {
		text = string(data)
	}
	v, err := Parse(text)
	if err != nil {
		return err
	}
	*m = v
	return nil
}
