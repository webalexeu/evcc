package core

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/hems/hems"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
)

// batteryLog is the dedicated "battery" log area for solar control and the fast loop,
// so battery lines can be filtered and level-controlled separately from "site".
var batteryLog = util.NewLogger("battery")

// chargeTaperRange is the SoC band (percentage points) before maxSoc in which the
// charge power is linearly tapered down to chargeMinFactor. This mimics the CC/CV
// profile that protects cells from stress near full charge. Some inverters enforce
// this internally; the software taper is a safety net when running in RS485 mode.
const (
	chargeTaperRange = 5.0  // begin tapering this many % below maxSoc
	chargeMinFactor  = 0.25 // taper down to 25% of requested power at maxSoc
	stopRefreshTicks = 10   // re-send stop to an already-stopped battery every N ticks (watchdog heartbeat)
	// batteryTierFraction sizes the tier count off a fraction of each battery's rated
	// charge/discharge power rather than the full rating, so load spreads onto more units
	// earlier (e.g. 2000W onto 2 of 3 at 50%). Spreading covers more phases (single-phase
	// inverters) and runs each unit at a more efficient partial load; the cost is each extra
	// inverter's standby draw. The per-battery power cap stays the full rating, so a unit can
	// still ramp to its limit when others drop out.
	batteryTierFraction = 0.5
)

// computeTier returns the number of batteries to activate given the current power target,
// per-battery rated power, the previously-used tier (for hysteresis), and the number
// of available batteries.
//
// Why tiering?
// Splitting a small target equally across all batteries often results in per-unit
// commands below the inverter's minimum effective power (e.g. Marstek ignores <50 W).
// Tiering uses the minimum number of batteries so each unit stays at or below its
// rated capacity — which also keeps each inverter at a more efficient operating point.
//
// Hysteresis (15% dead band):
// Without hysteresis the tier would flip on every tick when the target hovers near a
// boundary. The dead band means we switch up only when the target clearly exceeds the
// current tier's capacity (×1.15), and switch down only when it clearly falls below
// the previous tier's capacity (×0.85). Jumps of more than one tier skip the dead band
// and respond immediately.
func computeTier(target, maxPerBat float64, currentTier, maxTier int) int {
	if maxPerBat <= 0 || maxTier <= 1 {
		return maxTier
	}

	const hysteresis = 0.15

	// minimum batteries needed so each handles at most maxPerBat
	raw := int(math.Ceil(target / maxPerBat))
	if raw < 1 {
		raw = 1
	}
	if raw > maxTier {
		raw = maxTier
	}

	// first call: jump directly to the correct tier with no hysteresis
	if currentTier == 0 {
		return raw
	}

	diff := raw - currentTier
	if diff > 1 || diff < -1 {
		// large change (>1 tier): respond immediately without waiting for dead band
		return raw
	}

	// single-tier transitions: enforce hysteresis dead band
	if diff == 1 && target > float64(currentTier)*maxPerBat*(1+hysteresis) {
		return currentTier + 1 // clearly above current tier's capacity
	}
	if diff == -1 && target < float64(currentTier-1)*maxPerBat*(1-hysteresis) {
		return currentTier - 1 // clearly below previous tier's capacity
	}
	return currentTier
}

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

	batteryLog.DEBUG.Println("set battery mode:", batMode)

	if site.batteryMode != batMode {
		site.setBatteryMode(batMode)
	}

	if site.batteryModeExternal == api.BatteryUnknown {
		site.batteryModeExternalTimer = time.Time{}
	}
}

// fromTo reports whether the battery is entering, holding or leaving mode m:
// either m is requested, or nothing is requested and the battery is already in m.
func (site *Site) fromTo(requested, m api.BatteryMode) bool {
	return requested == m || requested == api.BatteryUnknown && site.batteryMode == m
}

func (site *Site) updateBatteryMode(batteryGridChargeActive, batteryGridDischargeActive bool, rate api.Rate, sitePower float64, sitePowerValid bool) {
	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, batteryGridDischargeActive, rate, sitePower)

	// put battery into hold mode when charging is active and HEMS dimmed
	if dimmed := hems.Dimmed(site.hems); site.fromTo(batteryMode, api.BatteryCharge) && dimmed != nil && *dimmed {
		batteryLog.DEBUG.Println("battery mode: HEMS dimmed")
		batteryMode = api.BatteryHold
	}

	// stop discharging to grid when HEMS curtailed production, but keep self-consumption
	if curtailed := hems.Curtailed(site.hems); site.fromTo(batteryMode, api.BatteryDischarge) && curtailed != nil && *curtailed {
		batteryLog.DEBUG.Println("battery mode: HEMS curtailed")
		batteryMode = api.BatteryNormal
	}

	// NOTE: applyBatteryMode is always called when charge or discharge mode is active to
	// validate max soc / min soc reserve
	if modeChanged := batteryMode != api.BatteryUnknown; modeChanged || site.batteryMode == api.BatteryCharge || site.batteryMode == api.BatteryDischarge {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			if modeChanged {
				site.SetBatteryMode(batteryMode)
			}
		} else {
			batteryLog.ERROR.Println("battery mode:", err)
		}
	}

	// Solar control: refresh the fast loop's snapshot (SoC/limits/caps + config). The fast
	// loop owns all power decisions off fresh grid/battery readings. When solar control is
	// off - or when a higher-precedence mode overrides it (grid charge, external/API control) -
	// clear the snapshot so the fast loop parks and the mode-based control owns the battery.
	_ = sitePowerValid
	extMode := site.GetBatteryModeExternal()
	site.Lock()
	extModeReset := extMode == api.BatteryUnknown && !site.batteryModeExternalTimer.IsZero()
	site.Unlock()
	overridden := batteryGridChargeActive || extMode != api.BatteryUnknown || extModeReset
	if site.batterySolarControl && !overridden {
		site.buildBatterySnapshot(rate)
	} else {
		site.batteryPlanMu.Lock()
		// Switching solar control fully off: the last active battery still holds the setpoint
		// the fast loop commanded (Normal mode only flips the mode register, not the power
		// register), so stop every battery once before parking. Under batteryPlanMu the fast
		// tick can't run concurrently, so the fast loop stays the sole power writer. When a
		// higher-precedence controller takes over instead (grid charge, external) it re-commands
		// power every tick, so leave the setpoint to it rather than fighting with a stop.
		if snap := site.batterySnapshot; snap != nil && !overridden {
			if site.batteryStopped == nil {
				site.batteryStopped = make(map[string]int)
			}
			site.stopBatteries(fastEntriesAll(snap))
			site.batteryChargeActive, site.batteryDischargeActive = nil, nil
			site.batteryChargeTier, site.batteryDischargeTier = 0, 0
			site.batteryFastDirection = batteryPlanIdle
		}
		site.batterySnapshot = nil
		site.batteryPlanMu.Unlock()
	}
}

// buildBatterySnapshot reads the slow-moving per-battery state (SoC, limits, power caps) and
// the current config, then publishes a snapshot for the fast loop under batteryPlanMu. It
// commands no power and chooses no direction - those are the fast loop's job.
func (site *Site) buildBatterySnapshot(rate api.Rate) {
	var evPower, evPowerFast float64
	for _, lp := range site.loadpoints {
		if !lp.IsHeating() {
			p := lp.GetChargePower()
			evPower += p
			if lp.GetStatus() != api.StatusA && lp.IsFastChargingActive() {
				evPowerFast += p
			}
		}
	}

	// auto-disable calibration charge once aggregate SoC reaches 100 %
	if site.batteryCalibrationCharge && site.battery.Soc >= 100 {
		site.Lock()
		site.batteryCalibrationCharge = false
		site.Unlock()
		site.publish(keys.BatteryCalibrationCharge, false)
		batteryLog.DEBUG.Printf("battery calibration charge complete (soc %.0f%%)", site.battery.Soc)
	}

	// charge ignores residualPower below prioritySoc (the energy-balance surplus already
	// does); discharge excludes fast/planned EV power (or all EV below bufferSoc)
	residual := site.GetResidualPower()
	chargeOffset := 0.0
	if site.battery.Soc >= site.prioritySoc {
		chargeOffset = residual
	}
	var dischargeEvExcluded float64
	if site.dischargeControlActive(rate) {
		dischargeEvExcluded = evPowerFast
	} else if !(site.bufferSoc > 0 && site.battery.Soc > site.bufferSoc) {
		dischargeEvExcluded = evPower
	}

	snap := &batterySnapshot{
		enabled:             true,
		pool:                site.batterySolarPool,
		tiering:             site.batterySolarTiering,
		sticky:              site.batterySolarSticky,
		tapering:            site.batterySolarTapering,
		calibration:         site.batteryCalibrationCharge,
		chargeOffset:        chargeOffset,
		dischargeOffset:     residual,
		dischargeEvExcluded: dischargeEvExcluded,
		threshold:           standbyPower + site.batteryControlDeadBand,
		created:             time.Now(),
	}

	for _, dev := range site.batteryMeters {
		ctrl, ok := api.Cap[api.BatteryPowerController](dev.Instance())
		if !ok {
			continue
		}
		e := batterySnapEntry{ctrl: ctrl, meter: dev.Instance(), name: dev.Config().Name}
		if bat, ok := api.Cap[api.Battery](dev.Instance()); ok {
			if soc, err := bat.Soc(); err == nil {
				e.soc, e.socOK = soc, true
			}
		}
		if limiter, ok := api.Cap[api.BatterySocLimiter](dev.Instance()); ok {
			e.hasSocLimit = true
			e.minSoc, e.maxSoc = limiter.GetSocLimits()
		}
		if limiter, ok := api.Cap[api.BatteryPowerLimiter](dev.Instance()); ok {
			e.chargeCap, e.dischargeCap = limiter.GetPowerLimits()
		}
		snap.batteries = append(snap.batteries, e)
	}

	site.batteryPlanMu.Lock()
	site.batterySnapshot = snap
	site.batteryPlanMu.Unlock()

	batteryLog.TRACE.Printf("battery snapshot: %d batteries soc=%.0f%% chargeOff=%.0fW dischargeOff=%.0fW evExcl=%.0fW threshold=%.0fW",
		len(snap.batteries), site.battery.Soc, chargeOffset, residual, dischargeEvExcluded, snap.threshold)
}

// requiredBatteryMode determines required battery mode based on grid charge/discharge, rate, and site power
func (site *Site) requiredBatteryMode(batteryGridChargeActive, batteryGridDischargeActive bool, rate api.Rate, sitePower float64) api.BatteryMode {
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
		// independent limits (buy vs feed-in rate) can both be active at once;
		// charge wins to avoid buying and immediately selling
		if batteryGridDischargeActive && batMode != api.BatteryCharge {
			batteryLog.WARN.Println("battery mode: grid charge and grid discharge both active, charge takes priority")
		}
		res = keepUnlessModified(api.BatteryCharge)
	case site.dischargeControlActive(rate) || (batteryGridDischargeActive && site.evFastChargingActive()):
		// hold wins over feed-in discharge; fast charging holds even without
		// batteryDischargeControl, selling while an EV fast-charges is worse
		res = keepUnlessModified(api.BatteryHold)
	case site.batterySolarControl:
		// fork solar control: keep RS485 enabled (Hold) so the fast loop owns power every
		// tick. Normal mode would disable RS485 between ticks and hand control back to the
		// inverter. Takes precedence over the upstream grid-discharge mode, an incompatible
		// whole-battery "sell" mode.
		res = keepUnlessModified(api.BatteryHold)
	case batteryGridDischargeActive:
		res = keepUnlessModified(api.BatteryDischarge)
	case batteryModeModified(batMode):
		res = api.BatteryNormal
	}

	return res
}

// batterySocLimitReached reports whether the battery has reached the soc bound
// that should stop the requested mode: the max soc when charging, or the min
// soc reserve when discharging to grid. A configured limit of 0 disables the
// respective check (max is also disabled at 100).
func (site *Site) batterySocLimitReached(dev config.Device[api.Meter], discharge bool) (bool, error) {
	// Calibration charge bypasses the maxSoc limit so the battery charges to 100 %;
	// the min-soc reserve on discharge is never bypassed.
	if !discharge && site.batteryCalibrationCharge {
		return false, nil
	}

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

	minSoc, maxSoc := batLimiter.GetSocLimits()

	if discharge {
		if minSoc > 0 && soc <= minSoc {
			batteryLog.DEBUG.Printf("battery %s: reserve soc reached (%.0f <= %.0f)", deviceTitleOrName(dev), soc, minSoc)
			return true, nil
		}
		return false, nil
	}

	if maxSoc > 0 && maxSoc < 100 && soc >= maxSoc {
		batteryLog.DEBUG.Printf("battery %s: limit soc reached (%.0f >= %.0f)", deviceTitleOrName(dev), soc, maxSoc)
		return true, nil
	}

	return false, nil
}

// applyBatteryMode applies the mode to each battery.
//
// A battery that reached the soc bound of the requested mode is held instead:
// the max soc when charging, the min soc reserve when discharging to grid. This
// is decided per device, so one battery reaching its bound does not force the
// others into hold.
func (site *Site) applyBatteryMode(mode api.BatteryMode) error {
	fromToCharge := site.fromTo(mode, api.BatteryCharge)
	fromToDischarge := site.fromTo(mode, api.BatteryDischarge)

	if site.batteryModeApplied == nil {
		site.batteryModeApplied = make(map[string]api.BatteryMode)
	}

	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		batCtrl, ok := api.Cap[api.BatteryController](meter)
		if !ok {
			continue
		}

		// per-device mode so one battery reaching its soc bound does not affect the others
		deviceMode := mode

		// hold at the soc bound of the requested mode (max soc for charge, min soc reserve for grid discharge)
		if (fromToCharge || fromToDischarge) && deviceMode != api.BatteryHold {
			hold, err := site.batterySocLimitReached(dev, fromToDischarge)
			if err != nil && !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
			if hold {
				deviceMode = api.BatteryHold
			}
		}

		// don't re-apply the mode the battery is already in
		name := dev.Config().Name
		if deviceMode == api.BatteryUnknown || deviceMode == site.batteryModeApplied[name] {
			continue
		}

		if !slices.Contains(batCtrl.BatteryModes(), deviceMode) {
			batteryLog.DEBUG.Printf("battery %s does not support mode: %s", deviceTitleOrName(dev), deviceMode)
			continue
		}

		if err := batCtrl.SetBatteryMode(deviceMode); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
			continue
		}

		site.batteryModeApplied[name] = deviceMode
		batteryLog.DEBUG.Printf("set battery %s mode: %s", deviceTitleOrName(dev), deviceMode)
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

// batteryGridDischargeActive reports whether the feed-in rate has reached the
// grid discharge limit; the opt-in gates both this and the optimizer's planning
func (site *Site) batteryGridDischargeActive(rate api.Rate) bool {
	if !site.GetBatteryGridDischarge() {
		return false
	}

	limit := site.GetBatteryGridDischargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value >= *limit
}

func (site *Site) dischargeControlActive(rate api.Rate) bool {
	if !site.GetBatteryDischargeControl() {
		return false
	}

	for _, lp := range site.activeLoadpoints() {
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

// evFastChargingActive reports whether any loadpoint is fast charging,
// regardless of the batteryDischargeControl opt-in
func (site *Site) evFastChargingActive() bool {
	for _, lp := range site.activeLoadpoints() {
		if lp.GetStatus() == api.StatusC && lp.IsFastChargingActive() {
			return true
		}
	}

	return false
}
