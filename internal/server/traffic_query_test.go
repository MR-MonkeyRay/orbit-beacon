package server

import "testing"

func TestTrafficDaysValidation(t *testing.T) {
	for _, test := range []struct {
		query string
		want  int
	}{{"", 30}, {"days=1", 1}, {"days=400", 400}, {"days=30", 30}} {
		days, fields := trafficDays(test.query)
		if days != test.want || len(fields) != 0 {
			t.Errorf("query=%q days=%d fields=%v", test.query, days, fields)
		}
	}
	for _, query := range []string{"days=", "days=0", "days=-1", "days=401", "days=1.5", "days=one", "days=999999999999999999999999", "days=1&days=2", "unknown=1", "days=%zz", "days=1;other=2"} {
		if _, fields := trafficDays(query); len(fields) == 0 {
			t.Errorf("accepted query=%q", query)
		}
	}
}
