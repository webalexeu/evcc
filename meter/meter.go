package meter

import (
	"context"
	"fmt"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/api/implement"
	"github.com/evcc-io/evcc/meter/measurement"
	"github.com/evcc-io/evcc/plugin"
	"github.com/evcc-io/evcc/util"
)

func init() {
	registry.AddCtx(api.Custom, NewConfigurableFromConfig)
}

// NewConfigurableFromConfig creates a new meter from config
func NewConfigurableFromConfig(ctx context.Context, other map[string]any) (api.Meter, error) {
	cc := struct {
		measurement.Energy    `mapstructure:",squash"` // energy optional
		measurement.Phases    `mapstructure:",squash"` // optional
		measurement.Dimmer    `mapstructure:",squash"` // optional
		measurement.Curtailer `mapstructure:",squash"` // optional

		// pv
		pvMaxACPower `mapstructure:",squash"`

		// battery
		batteryCapacity    `mapstructure:",squash"`
		batterySocLimits   `mapstructure:",squash"`
		batteryPowerLimits `mapstructure:",squash"`
		Soc                *plugin.Config // optional
		LimitSoc           *plugin.Config // optional
		BatteryMode        *plugin.Config // optional
		ChargePower        *plugin.Config // optional: dynamic charge power setter (watts)
		StopChargePower    *plugin.Config // optional: explicit stop-charge command
		DischargePower     *plugin.Config // optional: dynamic discharge power setter (watts)
	}{
		batterySocLimits: batterySocLimits{
			MinSoc: 20,
			MaxSoc: 95,
		},
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	powerG, energyG, err := cc.Energy.Configure(ctx)
	if err != nil {
		return nil, err
	}

	m, _ := NewConfigurable(powerG)
	implement.May(m, implement.MeterEnergy(energyG))

	// decorate soc
	socG, err := cc.Soc.FloatGetter(ctx)
	if err != nil {
		return nil, fmt.Errorf("battery soc: %w", err)
	}

	if socG != nil {
		implement.Has(m, implement.Battery(socG))
		implement.May(m, implement.BatteryCapacity(cc.batteryCapacity.Decorator()))
		implement.May(m, implement.BatterySocLimiter(cc.batterySocLimits.Decorator()))
		implement.May(m, implement.BatteryPowerLimiter(cc.batteryPowerLimits.Decorator()))

		switch {
		case cc.Soc != nil && cc.LimitSoc != nil:
			limitSocS, err := cc.LimitSoc.FloatSetter(ctx, "limitSoc")
			if err != nil {
				return nil, fmt.Errorf("battery limit soc: %w", err)
			}

			implement.Has(m, implement.BatteryController(cc.batterySocLimits.LimitController(socG, limitSocS)))

		case cc.BatteryMode != nil:
			modeS, err := cc.BatteryMode.IntSetter(ctx, "batteryMode")
			if err != nil {
				return nil, fmt.Errorf("battery mode: %w", err)
			}

			implement.Has(m, implement.BatteryController(func(mode api.BatteryMode) error {
				return modeS(int64(mode))
			}))
		}

		// wire dynamic charge/discharge power control if configured
		chargePowerS, err := cc.ChargePower.FloatSetter(ctx, "chargePower")
		if err != nil {
			return nil, fmt.Errorf("battery charge power: %w", err)
		}

		// optional explicit stop-charge command (replaces chargePower(0) on stop)
		stopChargePowerS, err := cc.StopChargePower.FloatSetter(ctx, "stopChargePower")
		if err != nil {
			return nil, fmt.Errorf("battery stop charge power: %w", err)
		}

		// wrap chargePowerS to use stopChargePowerS when watts == 0
		if chargePowerS != nil && stopChargePowerS != nil {
			rawCharge := chargePowerS
			chargePowerS = func(watts float64) error {
				if watts == 0 {
					return stopChargePowerS(0)
				}
				return rawCharge(watts)
			}
		}

		dischargePowerS, err := cc.DischargePower.FloatSetter(ctx, "dischargePower")
		if err != nil {
			return nil, fmt.Errorf("battery discharge power: %w", err)
		}

		// discharge(0) delegates to stopChargePowerS so the device receives an
		// explicit direction=Stop before the next charge command overwrites it.
		if dischargePowerS != nil {
			rawDischarge := dischargePowerS
			dischargePowerS = func(watts float64) error {
				if watts == 0 {
					if stopChargePowerS != nil {
						return stopChargePowerS(0)
					}
					return nil
				}
				return rawDischarge(watts)
			}
		}

		implement.May(m, implement.BatteryPowerController(chargePowerS, dischargePowerS))

		return m, nil
	}

	currentsG, voltagesG, powersG, err := cc.Phases.Configure(ctx)
	if err != nil {
		return nil, err
	}

	implement.May(m, implement.PhaseCurrents(currentsG))
	implement.May(m, implement.PhaseVoltages(voltagesG))
	implement.May(m, implement.PhasePowers(powersG))
	implement.May(m, implement.MaxACPowerGetter(cc.pvMaxACPower.Decorator()))

	// dim/curtail
	if err := cc.Dimmer.Implement(ctx, m); err != nil {
		return nil, err
	}
	if err := cc.Curtailer.Implement(ctx, m); err != nil {
		return nil, err
	}

	return m, nil
}

// NewConfigurable creates a new meter
func NewConfigurable(currentPowerG func() (float64, error)) (*Meter, error) {
	m := &Meter{
		Caps:          implement.New(),
		currentPowerG: currentPowerG,
	}
	return m, nil
}

// Meter is an api.Meter implementation with configurable getters and setters.
type Meter struct {
	implement.Caps
	currentPowerG func() (float64, error)
}

// CurrentPower implements the api.Meter interface
func (m *Meter) CurrentPower() (float64, error) {
	return m.currentPowerG()
}
