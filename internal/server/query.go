package server

import (
	"net/http"
	"net/url"
	"strconv"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/management"
)

type queryParameter struct {
	nonempty bool
	repeated string
}

func parseQuery(raw, malformed string, allowed map[string]queryParameter) (url.Values, map[string]string) {
	values, err := url.ParseQuery(raw)
	fields := make(map[string]string)
	if err != nil {
		fields["query"] = malformed
	}
	for key, items := range values {
		rule, ok := allowed[key]
		if !ok {
			fields[key] = "unknown parameter"
			continue
		}
		if len(items) != 1 || rule.nonempty && items[0] == "" {
			fields[key] = rule.repeated
		}
	}
	return values, fields
}

func parseLimit(values url.Values, fields map[string]string, defaultLimit int, message string) int {
	limit := defaultLimit
	items, ok := values["limit"]
	if !ok || len(items) != 1 || items[0] == "" {
		return limit
	}
	parsed, err := strconv.Atoi(items[0])
	if err != nil || parsed < 1 || parsed > 100 {
		fields["limit"] = message
		return limit
	}
	return parsed
}

func parseNonnegativeInt(values url.Values, fields map[string]string, key string, defaultValue int, message string) int {
	value := defaultValue
	items, ok := values[key]
	if !ok || len(items) != 1 || items[0] == "" {
		return value
	}
	parsed, err := strconv.Atoi(items[0])
	if err != nil || parsed < 0 {
		fields[key] = message
		return value
	}
	return parsed
}

func parseCursor(values url.Values, key string) (string, error) {
	cursor := values.Get(key)
	if cursor != "" {
		if _, _, err := management.DecodeEventCursor(cursor); err != nil {
			return "", err
		}
	}
	return cursor, nil
}

func pagination(r *http.Request) (int, int, error) {
	limit, offset, _, err := paginationQuery(r, false)
	return limit, offset, err
}

func paginationQuery(r *http.Request, beforeAllowed bool) (int, int, string, error) {
	const suppliedOnce = "must be supplied once and nonempty"
	allowed := map[string]queryParameter{
		"limit":  {nonempty: true, repeated: suppliedOnce},
		"offset": {nonempty: true, repeated: suppliedOnce},
	}
	if beforeAllowed {
		allowed["before"] = queryParameter{nonempty: true, repeated: suppliedOnce}
	}
	values, fields := parseQuery(r.URL.RawQuery, "invalid query string", allowed)
	limit := parseLimit(values, fields, 50, "must be an integer from 1 to 100")
	offset := parseNonnegativeInt(values, fields, "offset", 0, "must be a nonnegative integer")
	if len(fields) != 0 {
		return 0, 0, "", auth.ErrInvalidInput
	}
	before, err := parseCursor(values, "before")
	if err != nil {
		return 0, 0, "", auth.ErrInvalidInput
	}
	return limit, offset, before, nil
}
