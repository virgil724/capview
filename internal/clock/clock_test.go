//go:build windows || linux

package clock

import (
	"testing"
	"time"
)

func TestMonotonicAndFreq(t *testing.T) {
	a := Now()
	time.Sleep(20 * time.Millisecond)
	b := Now()
	ms := float64(b-a) * 1000 / float64(Freq())
	if ms < 15 || ms > 500 {
		t.Fatalf("20ms sleep measured as %.2fms (freq %d)", ms, Freq())
	}
}
