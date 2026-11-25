package audio

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMirrors starts stand-in mirror goroutines that run until stopped.
type fakeMirrors struct {
	started atomic.Int32
	mu      sync.Mutex
	all     []*mirrorState
}

func (f *fakeMirrors) start(id string) (*mirrorState, error) {
	capF, targetF, maxF := MirrorRingFrames(48000)
	st := &mirrorState{id: id, tap: newMirrorTap(NewRing(capF, targetF, maxF)), done: make(chan struct{})}
	go func() {
		defer close(st.done)
		for !st.stop.Load() {
			time.Sleep(50 * time.Microsecond)
		}
	}()
	f.started.Add(1)
	f.mu.Lock()
	f.all = append(f.all, st)
	f.mu.Unlock()
	return st, nil
}

func (f *fakeMirrors) allStopped(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range f.all {
		select {
		case <-st.done:
		case <-time.After(2 * time.Second):
			t.Fatalf("mirror %q still running after closeMirror", st.id)
		}
	}
}

func TestSetMirrorAfterCloseIsRejected(t *testing.T) {
	var f fakeMirrors
	p := &Passthrough{InputRate: 48000}
	if err := p.setMirror("cable", f.start); err != nil {
		t.Fatal(err)
	}
	if p.tap.Load() == nil {
		t.Fatal("tap not installed")
	}

	p.closeMirror()
	f.allStopped(t)
	if p.tap.Load() != nil || p.mir != nil {
		t.Fatal("closeMirror left mirror state behind")
	}

	if err := p.setMirror("cable", f.start); !errors.Is(err, ErrPassthroughStopped) {
		t.Fatalf("setMirror after close: got %v, want ErrPassthroughStopped", err)
	}
	if n := f.started.Load(); n != 1 {
		t.Fatalf("start called %d times, want 1", n)
	}
	if err := p.setMirror("", f.start); err != nil {
		t.Fatalf("setMirror(\"\") after close: %v", err)
	}
}

// A SetMirror racing with the passthrough's exit must either be rejected or
// have its mirror stopped by closeMirror; nothing may survive the close.
func TestSetMirrorRacingClose(t *testing.T) {
	for i := 0; i < 200; i++ {
		var f fakeMirrors
		p := &Passthrough{InputRate: 48000}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			err := p.setMirror("cable", f.start)
			if err != nil && !errors.Is(err, ErrPassthroughStopped) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			p.closeMirror()
		}()
		wg.Wait()
		f.allStopped(t)
		if p.mir != nil || p.tap.Load() != nil {
			t.Fatalf("iteration %d: mirror state survived close", i)
		}
		if err := p.setMirror("cable", f.start); !errors.Is(err, ErrPassthroughStopped) {
			t.Fatalf("iteration %d: setMirror after close: %v", i, err)
		}
	}
}

func TestSetMirrorRejectsMainOutput(t *testing.T) {
	var f fakeMirrors
	p := &Passthrough{InputRate: 48000, OutputID: "{0.0.0}.{SPEAKERS}"}
	if err := p.setMirror("cable", f.start); err != nil {
		t.Fatal(err)
	}
	if err := p.setMirror("{0.0.0}.{speakers}", f.start); !errors.Is(err, ErrMirrorIsMainOutput) {
		t.Fatalf("got %v, want ErrMirrorIsMainOutput", err)
	}
	if p.mir == nil || p.mir.id != "cable" {
		t.Fatal("running mirror should be left untouched")
	}
	p.closeMirror()
	f.allStopped(t)
}

func TestSetMirrorSwitchAndStop(t *testing.T) {
	var f fakeMirrors
	p := &Passthrough{InputRate: 48000}
	for _, id := range []string{"a", "A", "b"} {
		if err := p.setMirror(id, f.start); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.started.Load(); n != 2 { // "A" is the same endpoint as "a"
		t.Fatalf("start called %d times, want 2", n)
	}
	if err := p.setMirror("", f.start); err != nil {
		t.Fatal(err)
	}
	f.allStopped(t)
	if p.mir != nil || p.tap.Load() != nil {
		t.Fatal("stop left mirror state behind")
	}
}
