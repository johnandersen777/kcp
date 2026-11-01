/*
Copyright 2026 The kcp Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package reflector

import (
	"testing"
	"time"

	testingclock "k8s.io/utils/clock/testing"
)

func TestDefaultBackoffParameters(t *testing.T) {
	b := defaultBackoff()
	if b.Duration != 100*time.Millisecond {
		t.Errorf("initial backoff = %v, want 100ms", b.Duration)
	}
	if b.Cap != 30*time.Second {
		t.Errorf("cap = %v, want 30s (steady-state protection for an unhealthy apiserver)", b.Cap)
	}
	if b.Factor != 2.0 || b.Jitter != 1.0 {
		t.Errorf("factor/jitter = %v/%v, want 2/1", b.Factor, b.Jitter)
	}
	if defaultBackoffReset != 2*time.Minute {
		t.Errorf("reset = %v, want 2m", defaultBackoffReset)
	}
	if want := 300; b.Steps != want {
		t.Errorf("steps = %d, want %d (enough to reach the cap)", b.Steps, want)
	}
}

// TestDefaultBackoffSchedule checks the retry schedule of a reflector that
// keeps failing: [100,200) ms first, doubling, then pinned to [30,60) s. The
// backoff manager restarts the schedule once the reset interval has passed
// since it last restarted, so the fake clock stays put while it grows.
func TestDefaultBackoffSchedule(t *testing.T) {
	clock := testingclock.NewFakeClock(time.Now())
	next := defaultBackoff().DelayWithReset(clock, defaultBackoffReset)

	base := 100 * time.Millisecond
	for i := range 20 {
		d := next()
		if d < base || d >= 2*base {
			t.Fatalf("retry %d: delay %v outside [%v,%v)", i, d, base, 2*base)
		}
		base = min(2*base, 30*time.Second)
	}
	if base != 30*time.Second {
		t.Fatalf("schedule did not reach the 30s cap, at %v", base)
	}

	// The reset interval passed: start fast again.
	clock.Step(defaultBackoffReset + time.Second)
	if d := next(); d >= 200*time.Millisecond {
		t.Fatalf("delay after reset = %v, want < 200ms", d)
	}
}
