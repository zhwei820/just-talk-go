//go:build !(darwin && cgo)

package presence

import "time"

const supported = false

func idleTime() time.Duration { return 0 }
func nudge()                  {}
func holdAwake(bool) error    { return nil }
