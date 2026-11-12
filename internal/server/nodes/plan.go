package nodes

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/validation"
)

var pricePattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`)

// UnmarshalJSON rejects invalid raw UTF-8 before encoding/json can replace it.
func (p *MetadataPatch) UnmarshalJSON(data []byte) error {
	type wire MetadataPatch
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for key, value := range raw {
		if !utf8.Valid(value) {
			return validation.Invalid(key, "must be valid UTF-8")
		}
	}
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*p = MetadataPatch(value)
	return nil
}

// ValidatePlan is shared with backup validation; valid text is trimmed on PATCH.
func (p *MetadataPatch) ValidatePlan() error {
	fields := map[string]string{}
	if p.DisplayOrder != nil && (*p.DisplayOrder < -1000000 || *p.DisplayOrder > 1000000) {
		fields["display_order"] = "must be between -1000000 and 1000000"
	}
	for _, f := range []struct {
		name  string
		value *string
		max   int
	}{
		{"price_amount", p.PriceAmount, 16}, {"price_currency", p.PriceCurrency, 8}, {"billing_cycle", p.BillingCycle, 16},
		{"bandwidth", p.Bandwidth, 64}, {"network_route", p.NetworkRoute, 128}, {"plan_extra", p.PlanExtra, 255},
	} {
		if f.value == nil {
			continue
		}
		if !utf8.ValidString(*f.value) || strings.ContainsFunc(*f.value, unicode.IsControl) {
			fields[f.name] = "must be valid UTF-8 without control characters"
			continue
		}
		*f.value = strings.TrimSpace(*f.value)
		length := len(*f.value)
		if f.name == "price_currency" {
			length = utf8.RuneCountInString(*f.value)
		}
		if length > f.max {
			fields[f.name] = "text is too long"
		}
	}
	if p.PriceAmount != nil && *p.PriceAmount != "" && !pricePattern.MatchString(*p.PriceAmount) {
		fields["price_amount"] = "must be a non-negative decimal with at most two fraction digits"
	}
	if p.PriceCurrency != nil && strings.ContainsFunc(*p.PriceCurrency, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.Is(unicode.Sc, r) }) {
		fields["price_currency"] = "must contain only letters or currency symbols"
	}
	if p.BillingCycle != nil {
		switch *p.BillingCycle {
		case "", "month", "quarter", "half_year", "year", "once":
		default:
			fields["billing_cycle"] = "invalid billing cycle"
		}
	}
	if len(fields) > 0 {
		return &validation.Error{Fields: fields}
	}
	return nil
}

func metadataChanges(before, after Node) audit.Details {
	a, b := reflect.ValueOf(before), reflect.ValueOf(after)
	changed := audit.Details{}
	for i := 0; i < a.NumField(); i++ {
		if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
			changed[a.Type().Field(i).Tag.Get("json")] = b.Field(i).Interface()
		}
	}
	return changed
}
