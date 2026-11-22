package capture

import (
	"sync"
	"sync/atomic"
)

// Source is a running video capture (Media Foundation, V4L2 or synthetic).
type Source struct {
	stop    atomic.Bool
	done    chan struct{}
	errMu   sync.Mutex
	err     error
	status  atomic.Value // string ("", "reconnecting...")
	Mode    Mode
	Mailbox *Mailbox
}

func (s *Source) Done() <-chan struct{} { return s.done }
func (s *Source) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}
func (s *Source) Status() string {
	if v, ok := s.status.Load().(string); ok {
		return v
	}
	return ""
}
func (s *Source) Stop() { s.stop.Store(true) }
