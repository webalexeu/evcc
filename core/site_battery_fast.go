package core

import (
	"math"
	"time"

	"github.com/evcc-io/evcc/api"
)

// The battery fast loop is a thin companion to applyBatterySolarPower. All decisions
// (charge/discharge direction, tiering, sticky selection, swaps, stops, mode handling)
// remain in the main loop. The fast loop only re-scales the power commands of the
// currently active batteries against a fresh grid reading, closing the gap between
// main loop ticks. It never changes direction, never selects batteries and never
// sends stop commands - when its correction reaches zero it clamps and waits for
// the main loop to decide.
const (
	batteryFastLoopPeriod = time.Second      // grid meters typically refresh their registers at 1s
	batteryPlanMaxAge     = 30 * time.Second // ignore plans when the main loop stopped updating them
	fastLoopMinDelta      = 25.0             // W; skip Modbus writes for smaller corrections
	fastLoopGain          = 0.5              // damping: fraction of the correction applied per tick;
	// guards against overshoot while the inverter is still ramping toward the previous command
)

type batteryPlanDirection int

const (
	batteryPlanIdle batteryPlanDirection = iota
	batteryPlanCharge
	batteryPlanDischarge
)

type batteryPlanEntry struct {
	ctrl api.BatteryPowerController
	name string
	cap  float64 // effective per-battery power limit in W incl. taper; 0 = uncapped
}

// batteryControlPlan is the contract between the main loop and the fast loop.
// The main loop replaces it on every tick; the fast loop adjusts total between ticks.
// Both access it under batteryPlanMu.
type batteryControlPlan struct {
	direction  batteryPlanDirection
	entries    []batteryPlanEntry
	evExcluded float64 // W of EV charge power the battery must not cover (discharge only)
	total      float64 // currently commanded total power across entries
	created    time.Time
}

// batteryFastLoop runs the 1s correction ticker until stopC closes.
func (site *Site) batteryFastLoop(stopC chan struct{}) {
	if site.gridMeter == nil {
		return
	}

	ticker := time.NewTicker(batteryFastLoopPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stopC:
			return
		case <-ticker.C:
			site.batteryFastTick()
		}
	}
}

func (site *Site) batteryFastTick() {
	site.batteryPlanMu.Lock()
	defer site.batteryPlanMu.Unlock()

	plan := site.batteryPlan
	if plan == nil || plan.direction == batteryPlanIdle || len(plan.entries) == 0 ||
		time.Since(plan.created) > batteryPlanMaxAge {
		return
	}

	gridPower, err := site.gridMeter.CurrentPower()
	if err != nil {
		// skip tick, next attempt in one period
		site.log.DEBUG.Printf("solar power (fast): grid power: %v", err)
		return
	}

	// Incremental zero-grid correction using the commanded total as battery power proxy:
	// no battery or PV meter reads needed. Steady state (grid ≈ -residual) yields
	// target == plan.total, so the loop is quiescent until grid moves.
	residual := site.GetResidualPower()

	var correction float64
	switch plan.direction {
	case batteryPlanDischarge:
		correction = gridPower + residual - plan.evExcluded
	case batteryPlanCharge:
		correction = -(gridPower + residual)
	}
	target := math.Max(plan.total+fastLoopGain*correction, 0)

	if math.Abs(target-plan.total) < fastLoopMinDelta {
		return
	}

	share := target / float64(len(plan.entries))

	var commanded float64
	for _, e := range plan.entries {
		p := share
		if e.cap > 0 && p > e.cap {
			p = e.cap
		}

		if plan.direction == batteryPlanCharge {
			err = e.ctrl.SetBatteryChargePower(p)
		} else {
			err = e.ctrl.SetBatteryDischargePower(p)
		}
		if err != nil {
			site.log.ERROR.Printf("solar power (fast): %s: %v", e.name, err)
			continue
		}

		commanded += p
	}

	dir := map[batteryPlanDirection]string{batteryPlanCharge: "charge", batteryPlanDischarge: "discharge"}[plan.direction]
	site.log.DEBUG.Printf("solar power (fast): %s %.0fW → %.0fW (grid %.0fW)", dir, plan.total, commanded, gridPower)

	plan.total = commanded
}
