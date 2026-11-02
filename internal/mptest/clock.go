package mptest

import (
	"github.com/rgracey/kanban-mcp/internal/clocksource"
)

// clockFromEnvOrFile builds the parent's clock from the same seam the children
// use. The parent never mutates time except through WriteClockMS (§1.3).
func clockFromEnvOrFile(clockFile string) (clocksource.Clock, error) {
	if clockFile == "" {
		return clocksource.RealClock{}, nil
	}
	return clocksource.NewFakeClock(clockFile), nil
}
