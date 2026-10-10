// Package fmtutil has the small number/size/duration formatters shared by the
// CLI status lines and the report renderer.
package fmtutil

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Int renders 1234567 as "1,234,567".
func Int(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Float renders with thousands separators and the given decimals.
func Float(f float64, decimals int) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "n/a"
	}
	whole := Int(int64(math.Abs(f)))
	if decimals <= 0 {
		if f < 0 {
			return "-" + whole
		}
		return whole
	}
	frac := math.Abs(f) - math.Floor(math.Abs(f))
	fs := strconv.FormatFloat(frac, 'f', decimals, 64)[1:] // ".xx"
	if fs == "."+strings.Repeat("0", decimals) && frac >= 0.5 {
		// rounding carried into the integer part
		whole = Int(int64(math.Abs(f)) + 1)
	}
	if f < 0 {
		return "-" + whole + fs
	}
	return whole + fs
}

// Bytes renders a byte count with a binary-free, human unit (kB/MB/GB, 1000-based).
func Bytes(n int64) string {
	return BytesF(float64(n))
}

// BytesF is Bytes for floats (rates).
func BytesF(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "n/a"
	}
	abs := math.Abs(f)
	switch {
	case abs < 1000:
		return fmt.Sprintf("%.0f B", f)
	case abs < 1e6:
		return fmt.Sprintf("%.1f kB", f/1e3)
	case abs < 1e9:
		return fmt.Sprintf("%.1f MB", f/1e6)
	case abs < 1e12:
		return fmt.Sprintf("%.2f GB", f/1e9)
	default:
		return fmt.Sprintf("%.2f TB", f/1e12)
	}
}

// Rate renders a per-second count compactly: "1,234/s", "12.3k/s".
func Rate(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "n/a"
	}
	abs := math.Abs(f)
	switch {
	case abs < 10000:
		return Float(f, 0) + "/s"
	case abs < 1e6:
		return fmt.Sprintf("%.1fk/s", f/1e3)
	default:
		return fmt.Sprintf("%.2fM/s", f/1e6)
	}
}

// Duration renders durations without sub-millisecond noise: "1h02m", "45.3s", "120ms".
func Duration(d time.Duration) string {
	switch {
	case d < 0:
		return "-" + Duration(-d)
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// Pct renders a ratio (0..1) as a percentage with sensible precision.
func Pct(r float64) string {
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return "n/a"
	}
	p := r * 100
	switch {
	case p == 0:
		return "0%"
	case p >= 99.9 && p < 100:
		// Enough decimals to show a small loss, without trailing zeros.
		s := strings.TrimRight(fmt.Sprintf("%.5f", p), "0")
		if len(s)-strings.Index(s, ".") <= 2 {
			s = fmt.Sprintf("%.2f", p)
		}
		return s + "%"
	case p >= 10:
		return fmt.Sprintf("%.2f%%", p)
	case p >= 0.01:
		return fmt.Sprintf("%.3f%%", p)
	default:
		return fmt.Sprintf("%.5f%%", p)
	}
}

// ParseBytes accepts "512", "512KiB", "4MiB", "1MB", "2G", "1.5m" (case-insensitive).
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"tib", 1 << 40}, {"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10},
		{"tb", 1e12}, {"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3},
		{"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return int64(f * float64(mult)), nil
}
