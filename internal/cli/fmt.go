package cli

import (
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
)

func fmtInt(n int64) string   { return fmtutil.Int(n) }
func rate(f float64) string   { return fmtutil.Rate(f) }
func bytesF(f float64) string { return fmtutil.BytesF(f) }
func secs(s float64) string   { return fmtutil.Duration(time.Duration(s * float64(time.Second))) }
func pct(a, b int64) string {
	if b == 0 {
		return "n/a"
	}
	return fmtutil.Pct(float64(a) / float64(b))
}
