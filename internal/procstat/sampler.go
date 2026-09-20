// Package procstat reads this process's CPU time and resident memory, and
// turns successive readings into a CPU percentage.
package procstat

import (
	"sync"
	"time"
)

var procStart = time.Now()

// Sampler tracks CPU time between samples.
type Sampler struct {
	mu      sync.Mutex
	lastCPU float64
	lastT   time.Time
	sum     float64
	n       int64
	rssMax  int64
}

// Sample returns cumulative CPU seconds, CPU% since the previous sample (or
// since process start on the first call) and the current RSS.
func (p *Sampler) Sample() (cpuSeconds, cpuPercent float64, rss int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cpu := CPUSeconds()
	now := time.Now()
	if !p.lastT.IsZero() {
		if dt := now.Sub(p.lastT).Seconds(); dt > 0 {
			cpuPercent = (cpu - p.lastCPU) / dt * 100
		}
	} else if el := now.Sub(procStart).Seconds(); el > 0 {
		cpuPercent = cpu / el * 100
	}
	p.lastCPU, p.lastT = cpu, now
	rss = RSS()
	p.sum += cpuPercent
	p.n++
	if rss > p.rssMax {
		p.rssMax = rss
	}
	return cpu, cpuPercent, rss
}

// Averages returns the mean CPU% over all samples and the peak RSS.
func (p *Sampler) Averages() (cpuAvg float64, rssMax int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n > 0 {
		cpuAvg = p.sum / float64(p.n)
	}
	return cpuAvg, p.rssMax
}

// Reset clears the running averages (the last CPU reading is kept).
func (p *Sampler) Reset() {
	p.mu.Lock()
	p.sum, p.n, p.rssMax = 0, 0, 0
	p.mu.Unlock()
}
