package kwaakaorders

import "backend-core/internal/domain"

// pickTable chooses the pool table for a new order (ADR-049). Pure.
//
//	load[T] = posLoad[T] + inflight[T]          when the POS answered
//	load[T] = live[T]                           when posLoad is nil (POS silent)
//
// The least-loaded table wins, ties go to the smaller position (pool is given
// in position order). load > 0 on the winner means every table is busy: the
// order still goes, shared=true — the POS allows several open orders per table.
func pickTable(pool []domain.KwaakaPoolTable, posLoad map[string]int, inflight, live map[string]int) (tableID string, shared bool) {
	best, bestLoad := "", -1
	for _, t := range pool {
		var load int
		if posLoad != nil {
			load = posLoad[t.KwaakaTableID] + inflight[t.KwaakaTableID]
		} else {
			load = live[t.KwaakaTableID]
		}
		if bestLoad < 0 || load < bestLoad {
			best, bestLoad = t.KwaakaTableID, load
		}
	}
	return best, bestLoad > 0
}
