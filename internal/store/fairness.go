package store

import "sort"

type endpointAllocation struct {
	id       string
	capacity int
	active   int
	take     int
	lastTurn int
}

// allocateClaims balances live claims before rotating equal-occupancy endpoints.
// Input order breaks initial ties by durable service order. Group by the last
// allocated turn so an incomplete round rotates on the next claim call too.
func allocateClaims(endpoints []endpointAllocation, limit int) []endpointAllocation {
	for turn := 1; turn <= limit; turn++ {
		best := -1
		for i := range endpoints {
			ep := &endpoints[i]
			if ep.take == ep.capacity {
				continue
			}
			if best == -1 || ep.active+ep.take < endpoints[best].active+endpoints[best].take ||
				(ep.active+ep.take == endpoints[best].active+endpoints[best].take && ep.lastTurn < endpoints[best].lastTurn) {
				best = i
			}
		}
		if best == -1 {
			break
		}
		endpoints[best].take++
		endpoints[best].lastTurn = turn
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].lastTurn < endpoints[j].lastTurn })
	return endpoints
}
