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

	// put battery into hold mode when charging is active and HEMS dimmed
	fromToCharge := batteryMode == api.BatteryCharge || batteryMode == api.BatteryUnknown && site.batteryMode == api.BatteryCharge
	if dimmed := hemsDimmed(site.hems); fromToCharge && dimmed != nil && *dimmed {
		site.log.DEBUG.Println("battery mode: HEMS dimmed")
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
	// When battery has priority (soc below threshold), derive the true solar surplus
	// independent of what chargers and the battery are currently drawing:
	//   surplus = pvPower - houseLoad = -(gridPower - chargerLoad + batteryDischarge)
	// sitePower (= gridPower) already has battery discharge zeroed by site.sitePower()
	// when SOC < prioritySoc, so we subtract charger load and add back the discharge
	// that was zeroed, giving the real solar surplus without grid or battery contribution.
	// This adjustment is only applied to the charge signal (surplus): for discharge we use
	// raw sitePower so that EV loads in Fast/Min mode remain visible to the battery controller.
	sitePowerCharge := sitePower
	if site.battery.Soc < site.prioritySoc {
		batteryDischargePower := max(0, -site.battery.Power) // positive when discharging
		sitePowerCharge -= totalChargePower - batteryDischargePower
	}
	surplus := -sitePowerCharge // positive = exporting (solar surplus)

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
		site.log.DEBUG.Printf("solar power: charge %.0fW surplus across %d/%d batteries", surplus, len(active), len(all))

	case sitePower > standbyPower && !site.dischargeControlActive(rate):
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
		share := sitePower / float64(len(active))
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
		site.log.DEBUG.Printf("solar power: discharge %.0fW deficit across %d/%d batteries", sitePower, len(active), len(all))

	default:
		stopAll(all)
		site.log.DEBUG.Printf("solar power: balanced, stop")
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
		// fast/plan charging: prevent discharge whenever mode is active, regardless of
		// momentary EV status (StatusB during phase negotiation, ramp-up, etc.)
		if lp.IsFastChargingActive() {
			return true
		}
		// smart cost: only prevent discharge when current is actually flowing
		if lp.GetStatus() == api.StatusC && site.smartCostActive(lp, rate) {
			return true
		}
	}

	return false
}
