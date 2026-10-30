package mydrv

import "time"

// ProbeIdleAfter sets how long a pooled connection sits idle before it is
// probed, and returns the function that restores the default. Exported for the
// live tests, which cannot wait out a second per case.
func ProbeIdleAfter(d time.Duration) (restore func()) {
	old := idleProbeAfter
	idleProbeAfter = d
	return func() { idleProbeAfter = old }
}
