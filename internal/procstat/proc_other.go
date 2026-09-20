//go:build !unix

package procstat

func CPUSeconds() float64 { return 0 }
func RSS() int64          { return 0 }
