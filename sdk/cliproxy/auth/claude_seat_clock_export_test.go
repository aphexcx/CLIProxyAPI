package auth

import "time"

// SetClaudeSeatClockForTest moves the seat router's clock from an external test
// package, so tests that run the real executors never sleep.
func SetClaudeSeatClockForTest(m *Manager, now func() time.Time) {
	m.claudeSeats.now = now
}
