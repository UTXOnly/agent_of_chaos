package wire

import "testing"

func TestFindMarker(t *testing.T) {
	cases := []struct {
		in     string
		gen    string
		stream string
		seq    int64
		ok     bool
	}{
		{"2026-09-20T02:02:24.123456Z INFO     [aoc.auth-service] aoc=plain-01/auth-service/1234 Request done", "plain-01", "auth-service", 1234, true},
		{`{"timestamp":"2026-09-20T02:02:24.123456Z","level":"INFO","aoc":"json-x/db-proxy/7","service":"db-proxy"}`, "json-x", "db-proxy", 7, true},
		{"aoc=g/s/1", "g", "s", 1, true},
		{"aoc=g/s/", "", "", 0, false},
		{"aoc=/s/1", "", "", 0, false},
		{"no marker here", "", "", 0, false},
		{"aoc=g/s/12345678901234567890", "", "", 0, false},
		{"aoc=g/s/42\nTraceback (most recent call last):", "g", "s", 42, true},
	}
	for _, c := range cases {
		m, ok := FindMarker(c.in)
		if ok != c.ok || m.Gen != c.gen || m.Stream != c.stream || m.Seq != c.seq {
			t.Errorf("FindMarker(%q) = %+v,%v; want %s/%s/%d,%v", c.in, m, ok, c.gen, c.stream, c.seq, c.ok)
		}
	}
}
