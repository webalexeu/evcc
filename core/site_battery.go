package core

import (
	"errors"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/util/config"
)

func batteryModeModified(mode api.BatteryMode) bool {
	return mode != api.BatteryUnknown && mode != api.BatteryNormal
}

func (site *Site) batteryConfigured() bool {
	return len(site.batteryMeters) > 0
}

func (site *Site) hasBatteryControl() bool {
	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		if api.HasCap[api.BatteryController](meter) {
			return true
		}
	}

	return false
}

// setBatteryMode sets the battery mode
func (site *Site) setBatteryMode(batMode api.BatteryMode) {
	site.batteryMode = batMode
	site.publish(keys.BatteryMode, batMode)
}

// SetBatteryMode sets the battery mode
func (site *Site) SetBatteryMode(batMode api.BatteryMode) {
	site.Lock()
	defer site.Unlock()

	site.log.DEBUG.Println("set battery mode:", batMode)

	if site.batteryMode != batMode {
		site.setBatteryMode(batMode)
	}

	if site.batteryModeExternal == api.BatteryUnknown {
		site.batteryModeExternalTimer = time.Time{}
	}
}

func (site *Site) updateBatteryMode(batteryGridChargeActive bool, rate api.Rate, sitePower, totalChargePower float64) {
	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, rate, sitePower)

	// put battery into hold mode when charging is active and circuit dimmed
	fromToCharge := batteryMode == api.BatteryCharge || batteryMode == api.BatteryUnknown && site.batteryMode == api.BatteryCharge
	if dimmed := circuitDimmed(site.circuit); fromToCharge && dimmed != nil && *dimmed {
		site.log.DEBUG.Println("battery mode: circuit dimmed")
		batteryMode = api.BatteryHold
	}

	// NOTE: applyBatteryMode is always called when charge mode is active to validate max soc
	if modeChanged := batteryMode != api.BatteryUnknown; modeChanged || site.batteryMode == api.BatteryCharge {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			if modeChanged {
				site.SetBatteryMode(batteryMode)
			}
		} else {
			site.log.ERROR.Println("battery mode:", err)
		}
	}

	// when solar control is active, drive power-level setters on capable battery meters
	if site.batterySolarControl {
		site.applyBatterySolarPower(rate, sitePower, totalChargePower)
	}
}

// applyBatterySolarPower calls SetBatteryChargePower / SetBatteryDischargePower on each battery
// meter that implements BatteryPowerController, proportional to the solar surplus or deficit.
func (site *Site) applyBatterySolarPower(rate api.Rate, sitePower, totalChargePower float64) {
	// When battery has priority (soc below threshold), use the raw grid reading as the
	// surplus signal so the battery charges from actual solar export regardless of the
	// sitePower adjustment that throttles loadpoints. In normal mode sitePower is used
	// directly (negative = exporting = surplus available for battery).
	surplus := -sitePower // positive = exporting (solar surplus)
	if site.battery.Soc < site.prioritySoc {
		// gridPower already reflects battery discharge, so subtract it to get true solar surplus:
		// true_surplus = pvPower - housePower - EV = batteryPower - gridPower (from energy balance)
		// Using -gridPower alone overshoots by the amount batteries were discharging.
		surplus = site.battery.Power - site.gridPower
	}

	type entry struct {
		ctrl api.BatteryPowerController
		dev  config.Device[api.Meter]
	}

	// collect all capable controllers
	var all []entry
	for _, dev := range site.batteryMeters {
		if ctrl, ok := api.Cap[api.BatteryPowerController](dev.Instance()); ok {
			all = append(all, entry{ctrl, dev})
		}
	}
	if len(all) == 0 {
		return
	}

	stopAll := func(entries []entry) {
		for _, e := range entries {
			if err := e.ctrl.SetBatteryChargePower(0); err != nil {
				site.log.ERROR.Printf("battery charge power: %v", err)
			}
			if err := e.ctrl.SetBatteryDischargePower(0); err != nil {
				site.log.ERROR.Printf("battery discharge power: %v", err)
			}
		}
	}

	// read per-device SoC; returns 0 and ok=false when not available
	deviceSoc := func(dev config.Device[api.Meter]) (float64, bool) {
		bat, ok := api.Cap[api.Battery](dev.Instance())
		if !ok {
			return 0, false
		}
		soc, err := bat.Soc()
		return soc, err == nil
	}

	inPriorityMode := site.battery.Soc < site.prioritySoc

	switch {
	case surplus > standbyPower:
		// filter to batteries that have not yet reached their max SoC
		var active, full []entry
		for _, e := range all {
			soc, ok := deviceSoc(e.dev)
			if limiter, hasLimiter := api.Cap[api.BatterySocLimiter](e.dev.Instance()); ok && hasLimiter {
				if _, maxSoc := limiter.GetSocLimits(); maxSoc > 0 && soc >= maxSoc {
					full = append(full, e)
					continue
				}
			}
			active = append(active, e)
		}
		stopAll(full)
		if len(active) == 0 {
			stopAll(all)
			break
		}
		share := surplus / float64(len(active))
		// when share is below the minimum effective power, concentrate on the lowest-SoC battery
		// to avoid sending commands too small for the inverter to act on (e.g. Marstek ignores <50W)
		const minEffectiveShare = 50.0
		if share < minEffectiveShare && len(active) > 1 {
			bestIdx, bestSoc := 0, 101.0
			for i, e := range active {
				if soc, ok := deviceSoc(e.dev); ok && soc < bestSoc {
					bestSoc, bestIdx = soc, i
				}
			}
			var others []entry
			for i, e := range active {
				if i != bestIdx {
					others = append(others, e)
				}
			}
			stopAll(others)
			active = active[bestIdx : bestIdx+1]
			share = surplus
			site.log.DEBUG.Printf("solar power: share %.0fW below %.0fW min, concentrating on lowest-soc battery (%.0f%%)", surplus/float64(len(all)-len(full)), minEffectiveShare, bestSoc)
		}
		for _, e := range active {
			chargePower := share
			if limiter, ok := api.Cap[api.BatteryPowerLimiter](e.dev.Instance()); ok {
				if maxCharge, _ := limiter.GetPowerLimits(); maxCharge > 0 && chargePower > maxCharge {
					chargePower = maxCharge
				}
			}
			if err := e.ctrl.SetBatteryChargePower(chargePower); err != nil {
				site.log.ERROR.Printf("battery charge power: %v", err)
			}
		}
		site.log.DEBUG.Printf("solar power: charge %.0fW across %d/%d batteries", share*float64(len(active)), len(active), len(all))

	case !inPriorityMode && sitePower > standbyPower:
		// exclude EV charger load from discharge target when battery-supported is not active
		// (bufferSoc not configured or SoC below threshold) or discharge control is active
		batteryBuffered := site.bufferSoc > 0 && site.battery.Soc > site.bufferSoc
		dischargeTarget := sitePower
		if !batteryBuffered || site.dischargeControlActive(rate) {
			dischargeTarget = sitePower - totalChargePower
		}
		if dischargeTarget <= standbyPower {
			stopAll(all)
			site.log.DEBUG.Printf("solar power: discharge prevented (EV deficit only), stop")
			break
		}
		// filter to batteries that have not yet reached their min SoC
		var active, empty []entry
		for _, e := range all {
			soc, ok := deviceSoc(e.dev)
			if limiter, hasLimiter := api.Cap[api.BatterySocLimiter](e.dev.Instance()); ok && hasLimiter {
				if minSoc, _ := limiter.GetSocLimits(); soc <= minSoc {
					empty = append(empty, e)
					continue
				}
			}
			active = append(active, e)
		}
		stopAll(empty)
		if len(active) == 0 {
			stopAll(all)
			break
		}
		share := dischargeTarget / float64(len(active))
		for _, e := range active {
			dischargePower := share
			if limiter, ok := api.Cap[api.BatteryPowerLimiter](e.dev.Instance()); ok {
				if _, maxDischarge := limiter.GetPowerLimits(); maxDischarge > 0 && dischargePower > maxDischarge {
					dischargePower = maxDischarge
				}
			}
			if err := e.ctrl.SetBatteryDischargePower(dischargePower); err != nil {
				site.log.ERROR.Printf("battery discharge power: %v", err)
			}
		}
		site.log.DEBUG.Printf("solar power: discharge %.0fW deficit across %d/%d batteries", dischargeTarget, len(active), len(all))

	case inPriorityMode && site.gridPower > standbyPower:
		// grid is importing while battery has priority — stop to avoid oscillation
		stopAll(all)
		site.log.DEBUG.Printf("solar power: priority mode, grid import %.0fW, stop", site.gridPower)

	default:
		if inPriorityMode {
			// grid is within deadband — hold last command, no oscillation
			site.log.DEBUG.Printf("solar power: priority mode, balanced (grid %.0fW), hold", site.gridPower)
		} else {
			stopAll(all)
			site.log.DEBUG.Printf("solar power: balanced, stop")
		}
	}
}

// requiredBatteryMode determines required battery mode based on grid charge, rate, and site power
func (site *Site) requiredBatteryMode(batteryGridChargeActive bool, rate api.Rate, sitePower float64) api.BatteryMode {
	var res api.BatteryMode
	batMode := site.GetBatteryMode()
	extMode := site.GetBatteryModeExternal()

	var extModeReset bool
	if extMode == api.BatteryUnknown {
		site.Lock()
		extModeReset = !site.batteryModeExternalTimer.IsZero()
		site.Unlock()
	}

	keepUnlessModified := func(s api.BatteryMode) api.BatteryMode {
		return map[bool]api.BatteryMode{false: s, true: api.BatteryUnknown}[batMode == s]
	}

	switch {
	case !site.batteryConfigured():
		res = api.BatteryUnknown
	case extModeReset:
		// require normal mode to leave external control
		res = api.BatteryNormal
	case extMode != api.BatteryUnknown:
		// require external mode only once
		if extMode != batMode {
			res = extMode
		}
	case batteryGridChargeActive:
		res = keepUnlessModified(api.BatteryCharge)
	case site.dischargeControlActive(rate):
		res = keepUnlessModified(api.BatteryHold)
	case site.batterySolarControl:
		// Battery control: keep RS485 enabled (Hold) so applyBatterySolarPower owns every tick.
		// Normal mode would disable RS485 between ticks and hand control back to the inverter.
		res = keepUnlessModified(api.BatteryHold)
	case batteryModeModified(batMode):
		res = api.BatteryNormal
	}

	return res
}

// batteryMaxSocReached checks is battery has exceed max soc limit
func (site *Site) batteryMaxSocReached(dev config.Device[api.Meter]) (bool, error) {
	meter := dev.Instance()

	batLimiter, ok := api.Cap[api.BatterySocLimiter](meter)
	if !ok {
		return false, nil
	}

	batSoc, ok := api.Cap[api.Battery](meter)
	if !ok {
		return false, errors.New("battery with soc limits must have soc")
	}

	soc, err := batSoc.Soc()
	if err != nil {
		return false, err
	}

	if _, max := batLimiter.GetSocLimits(); max > 0 && max < 100 && soc >= max {
		site.log.DEBUG.Printf("battery %s: limit soc reached (%.0f > %.0f)", deviceTitleOrName(dev), soc, max)
		return true, nil
	}

	return false, nil
}

// applyBatteryMode applies the mode to each battery
//
// api.BatteryCharge:
//
//	The current soc is validated against max soc.
//	In case max soc is reached, hold mode is applied.
func (site *Site) applyBatteryMode(mode api.BatteryMode) error {
	fromToCharge := mode == api.BatteryCharge || mode == api.BatteryUnknown && site.batteryMode == api.BatteryCharge

	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		batCtrl, ok := api.Cap[api.BatteryController](meter)
		if !ok {
			continue
		}

		// validate max soc
		if fromToCharge && mode != api.BatteryHold {
			ok, err := site.batteryMaxSocReached(dev)
			if err != nil && !errors.Is(err, api.ErrNotAvailable) {
				return err
			}

			// put battery into hold mode when soc limit reached
			if ok {
				// TODO do this only once
				mode = api.BatteryHold
			}
		}

		if mode != api.BatteryUnknown {
			if err := batCtrl.SetBatteryMode(mode); err == nil {
				site.log.DEBUG.Printf("set battery %s mode: %s", deviceTitleOrName(dev), mode)
			} else if !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
		}
	}

	return nil
}

func (site *Site) tariffRates(usage api.TariffUsage) (api.Rates, error) {
	tariff := site.GetTariff(usage)
	if tariff == nil || tariff.Type() == api.TariffTypePriceStatic {
		return nil, nil
	}

	return tariff.Rates()
}

func (site *Site) smartCostActive(lp loadpoint.API, rate api.Rate) bool {
	limit := lp.GetSmartCostLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

func (site *Site) batteryGridChargeActive(rate api.Rate) bool {
	limit := site.GetBatteryGridChargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

func (site *Site) dischargeControlActive(rate api.Rate) bool {
	if !site.GetBatteryDischargeControl() {
		return false
	}

	for _, lp := range site.Loadpoints() {
		// fast/plan charging: car must be connected (StatusB+) but StatusC not required
		// so phase negotiation / ramp-up transitions don't momentarily re-enable discharge
		if lp.GetStatus() != api.StatusA && lp.IsFastChargingActive() {
			return true
		}
		// smart cost: only prevent discharge when current is actually flowing
		if lp.GetStatus() == api.StatusC && site.smartCostActive(lp, rate) {
			return true
		}
	}

	return false
}
