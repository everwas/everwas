package sched

import (
	"hash/fnv"
	"time"
)

// MisfireJitterS spreads a burst of catch-up runs after a fleet-wide outage.
const MisfireJitterS = 60

// Jitter returns this agent's stable offset for an entry. The same agent and
// entry always produce the same offset, so a 1000-host fleet spreads itself
// across the window identically every night — which makes a slow schedule
// reproducible instead of a new random mystery each run.
func Jitter(agentID, entryID string, jitterS int) time.Duration {
	if jitterS <= 0 {
		return 0
	}
	h := fnv.New64()
	// Writing to an fnv hash never fails.
	_, _ = h.Write([]byte(agentID))
	_, _ = h.Write([]byte(entryID))
	return time.Duration(h.Sum64()%uint64(jitterS)) * time.Second
}
