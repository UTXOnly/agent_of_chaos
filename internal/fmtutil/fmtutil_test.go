package fmtutil

import (
	"testing"
	"time"
)

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{"512": 512, "512KiB": 512 << 10, "4MiB": 4 << 20, "1MB": 1e6, "2g": 2 << 30, "1.5m": 1536 << 10, " 8 kb ": 8000}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseBytes("lots"); err == nil {
		t.Error("expected error for 'lots'")
	}
}

func TestFormatting(t *testing.T) {
	if Int(1234567) != "1,234,567" || Int(-42) != "-42" || Int(999) != "999" {
		t.Errorf("Int: %s %s %s", Int(1234567), Int(-42), Int(999))
	}
	if Bytes(1536) != "1.5 kB" || BytesF(2.5e6) != "2.5 MB" {
		t.Errorf("Bytes: %s %s", Bytes(1536), BytesF(2.5e6))
	}
	if Rate(1234) != "1,234/s" || Rate(52300) != "52.3k/s" {
		t.Errorf("Rate: %s %s", Rate(1234), Rate(52300))
	}
	if Duration(90*time.Second) != "1m30s" || Duration(250*time.Millisecond) != "250ms" {
		t.Errorf("Duration: %s %s", Duration(90*time.Second), Duration(250*time.Millisecond))
	}
	if Pct(1) != "100.00%" || Pct(0.99999) != "99.999%" || Pct(0) != "0%" {
		t.Errorf("Pct: %s %s %s", Pct(1), Pct(0.99999), Pct(0))
	}
}
