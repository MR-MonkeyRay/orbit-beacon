package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"orbitbeacon/internal/server/management"
)

func TestPaginationQueryValidation(t *testing.T) {
	cursor := management.EncodeEventCursor(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), "00000000-0000-4000-8000-000000000001")
	for _, test := range []struct {
		query  string
		before bool
		limit  int
		offset int
		cursor string
	}{
		{"", false, 50, 0, ""},
		{"limit=1&offset=2", false, 1, 2, ""},
		{"limit=100&offset=3&before=" + cursor, true, 100, 3, cursor},
	} {
		r := httptest.NewRequest("GET", "/?"+test.query, nil)
		limit, offset, cursor, err := paginationQuery(r, test.before)
		if err != nil || limit != test.limit || offset != test.offset || cursor != test.cursor {
			t.Errorf("query=%q got=(%d,%d,%q,%v)", test.query, limit, offset, cursor, err)
		}
	}

	for _, test := range []struct {
		query  string
		before bool
	}{
		{"unknown=1", false},
		{"limit=0", false},
		{"limit=101", false},
		{"offset=-1", false},
		{"before=invalid", true},
		{"limit=1&limit=2", false},
		{"offset=", false},
		{"before=cursor", false},
		{"before=one&before=two", true},
		{"%zz=x", false},
	} {
		r := httptest.NewRequest("GET", "/?"+test.query, nil)
		_, _, _, err := paginationQuery(r, test.before)
		if err == nil {
			t.Errorf("query=%q err=%v", test.query, err)
		}
	}
}
