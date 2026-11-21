package delivery

import "sync"

// Signal wakes every loop waiting on it at once (a broadcast). A loop takes C() before it
// looks for work and waits on that channel afterwards, so a Notify that lands in between is
// not missed. A nil *Signal never fires.
type Signal struct {
	mu sync.Mutex
	ch chan struct{}
}

// NewSignal returns a ready Signal.
func NewSignal() *Signal { return &Signal{ch: make(chan struct{})} }

// C returns a channel that the next Notify closes.
func (s *Signal) C() <-chan struct{} {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ch
}

// Notify wakes everything waiting on a channel returned by C.
func (s *Signal) Notify() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.ch)
	s.ch = make(chan struct{})
}
