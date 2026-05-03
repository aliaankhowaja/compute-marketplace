// Package lightningwallet provides shared prepaid balance UX hints for Lightning agents.
package lightningwallet

import "github.com/aliaankhowaja/sockt-backend/internal/config"

// Hints maps agent balance vs configured low-water / chunk to polling-friendly next actions.
func Hints(balanceMsats int64, rateMsatsPerSec int64, cfg *config.Control) (nextAction string, minDepositMsats int64, suggestedDepositMsats int64) {
	if cfg == nil {
		cfg = &config.Control{BalanceLowWaterSecs: 60, BalanceTopupChunkSecs: 300}
	}
	lwSecs := cfg.BalanceLowWaterSecs
	if lwSecs <= 0 {
		lwSecs = 60
	}
	chSecs := cfg.BalanceTopupChunkSecs
	if chSecs <= 0 {
		chSecs = 300
	}

	r := rateMsatsPerSec
	if r <= 0 {
		r = 1
	}
	lowWater := int64(lwSecs) * r
	chunk := int64(chSecs) * r

	minDepositMsats = r
	if minDepositMsats < 1000 {
		minDepositMsats = 1000
	}
	suggestedDepositMsats = chunk
	if suggestedDepositMsats < minDepositMsats {
		suggestedDepositMsats = minDepositMsats
	}

	switch {
	case balanceMsats <= 0:
		return "out_of_balance", minDepositMsats, suggestedDepositMsats
	case balanceMsats < lowWater:
		return "deposit_required", minDepositMsats, suggestedDepositMsats
	default:
		return "ok", minDepositMsats, suggestedDepositMsats
	}
}
