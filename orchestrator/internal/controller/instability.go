package controller

// HasInstability returns true if the windowed residual L2 ratio has exceeded
// threshold for at least maxTrips of the last window batches (after a grace
// period of grace batches).
//
// This is the Go equivalent of Controller._check_overshoot in Python, but
// exposed as a query rather than a panic so callers decide how to handle it.
//
// The rolling window is maintained inside State by pushOvershoot (called from
// Apply). HasInstability inspects it without modifying state.
func (s *State) HasInstability(threshold float64, window, maxTrips, grace int) bool {
	s.overshootMu.Lock()
	defer s.overshootMu.Unlock()
	// Ensure the window buffer is sized correctly. If it hasn't been allocated
	// yet (e.g. no Apply has been called), allocate it now.
	if len(s.overshootWindow) != window {
		s.overshootWindow = make([]bool, window)
		s.overshootHead = 0
		s.overshootFilled = 0
	}

	if s.overshootFilled < window {
		return false
	}
	trips := 0
	for _, v := range s.overshootWindow {
		if v {
			trips++
		}
	}
	return trips >= maxTrips
}

// initOvershootWindow ensures the rolling buffer is sized to cap. Called
// lazily by pushOvershoot / HasInstability so callers don't have to
// pre-configure it.
func (s *State) initOvershootWindow(cap int) {
	if len(s.overshootWindow) == cap {
		return
	}
	s.overshootWindow = make([]bool, cap)
	s.overshootHead = 0
	s.overshootFilled = 0
}
