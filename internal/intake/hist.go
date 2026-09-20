package intake

import (
	"math"
	"sort"
	"time"
)

// hist is a log-scale histogram: bucket i covers [lo·r^i, lo·r^(i+1)). With
// r = 1.04 quantiles are within ±2%, which is plenty for latency reporting
// and costs a few KB per histogram.
type hist struct {
	lo, logR float64
	counts   []uint64
	count    uint64
	sum      float64
	max      float64
	min      float64
}

func newHist(lo, hi, ratio float64) *hist {
	n := int(math.Ceil(math.Log(hi/lo)/math.Log(ratio))) + 2
	return &hist{lo: lo, logR: math.Log(ratio), counts: make([]uint64, n), min: math.Inf(1)}
}

// newLatencyHist covers 50µs .. 1h at ±2%.
func newLatencyHist() *hist { return newHist(50e-6, 3600, 1.04) }

// newSizeHist covers 1 byte .. 1 GB at ±5%.
func newSizeHist() *hist { return newHist(1, 1e9, 1.10) }

func (h *hist) bucket(v float64) int {
	if v < h.lo {
		return 0
	}
	i := int(math.Log(v/h.lo)/h.logR) + 1
	if i >= len(h.counts) {
		i = len(h.counts) - 1
	}
	return i
}

func (h *hist) add(v float64) {
	if v < 0 {
		v = 0
	}
	h.counts[h.bucket(v)]++
	h.count++
	h.sum += v
	if v > h.max {
		h.max = v
	}
	if v < h.min {
		h.min = v
	}
}

func (h *hist) addDuration(d time.Duration) { h.add(d.Seconds()) }

func (h *hist) merge(o *hist) {
	if o == nil {
		return
	}
	for i := range h.counts {
		if i < len(o.counts) {
			h.counts[i] += o.counts[i]
		}
	}
	h.count += o.count
	h.sum += o.sum
	if o.max > h.max {
		h.max = o.max
	}
	if o.min < h.min {
		h.min = o.min
	}
}

func (h *hist) reset() {
	for i := range h.counts {
		h.counts[i] = 0
	}
	h.count, h.sum, h.max, h.min = 0, 0, 0, math.Inf(1)
}

// upper returns the upper edge of bucket i.
func (h *hist) upper(i int) float64 {
	if i == 0 {
		return h.lo
	}
	return h.lo * math.Exp(float64(i)*h.logR)
}

// quantile returns an approximate q-quantile (0..1) of the observed values.
func (h *hist) quantile(q float64) float64 {
	if h.count == 0 {
		return 0
	}
	target := uint64(math.Ceil(q * float64(h.count)))
	if target == 0 {
		target = 1
	}
	var cum uint64
	for i, c := range h.counts {
		cum += c
		if cum >= target {
			u := h.upper(i)
			if u > h.max {
				u = h.max
			}
			return u
		}
	}
	return h.max
}

func (h *hist) mean() float64 {
	if h.count == 0 {
		return 0
	}
	return h.sum / float64(h.count)
}

// Quantiles is the exported summary of a histogram.
type Quantiles struct {
	Count uint64  `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	P999  float64 `json:"p999"`
	Max   float64 `json:"max"`
	Min   float64 `json:"min"`
}

func (h *hist) summary() Quantiles {
	if h == nil || h.count == 0 {
		return Quantiles{}
	}
	return Quantiles{
		Count: h.count, Mean: h.mean(),
		P50: h.quantile(0.5), P90: h.quantile(0.9), P99: h.quantile(0.99), P999: h.quantile(0.999),
		Max: h.max, Min: h.min,
	}
}

// topN returns the n largest entries of a count map.
type nameCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes,omitempty"`
}

func topN(m map[string]*nameCount, n int) []nameCount {
	out := make([]nameCount, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
