package engine

// LimitError is a configured ceiling being hit. The engine's own ceilings are
// the running count (MaxRunning), the running-memory budget and the
// concurrent-boot cap (admission.go); a front end reports its own through the
// same type.
type LimitError struct {
	// Code is the front end's error code for the ceiling. The engine leaves it
	// empty: a front end picks its code by Which.
	Code    string
	Message string
	// Which names the ceiling for the limit.refused event (docs/events.md).
	// Several ceilings may share one front-end Code, so this is what tells them apart.
	Which      string
	Limit      int
	Current    int
	RetryAfter int // seconds; 0 when waiting will not help
}

// Error is the message, for the log.
func (e *LimitError) Error() string { return e.Message }
