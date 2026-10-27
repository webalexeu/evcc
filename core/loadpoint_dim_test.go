package core

import (
	"testing"

	evbus "github.com/asaskevich/EventBus"
	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// A circuit with no remaining headroom is at its limit regardless of whether
// power or current is the constraining dimension; one with headroom is not.
func TestCircuitAtLimit(t *testing.T) {
	for _, tc := range []struct {
		name            string
		circuit         bool
		powerHeadroom   float64
		currentHeadroom float64
		want            bool
	}{
		{"no circuit", false, 0, 0, false},
		{"headroom on both dimensions", true, 10, 10, false},
		{"power at limit", true, -1, 10, true},
		{"current at limit, power ok", true, 10, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			lp := &Loadpoint{
				log:            util.NewLogger("foo"),
				status:         api.StatusC,
				chargePower:    100,
				offeredCurrent: 6,
			}

			if tc.circuit {
				circuit := api.NewMockCircuit(ctrl)
				circuit.EXPECT().ValidatePower(gomock.Any(), gomock.Any()).DoAndReturn(func(old, new float64) float64 {
					return min(new, old+tc.powerHeadroom)
				}).AnyTimes()
				circuit.EXPECT().ValidateCurrent(gomock.Any(), gomock.Any()).DoAndReturn(func(old, new float64) float64 {
					return min(new, old+tc.currentHeadroom)
				}).AnyTimes()
				lp.circuit = circuit
			}

			require.Equal(t, tc.want, lp.circuitAtLimit())
		})
	}
}

// A Dimmer-capable charger must dim once its circuit is saturated, independent of
// any HEMS/§14a signal, and must undim again once the circuit frees up (#35103-like
// stale-dim bugs: a root circuit may still have headroom while this loadpoint's own
// branch is overloaded, and vice versa).
func TestLoadpointUpdateDim(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hemsDim   *bool
		noCircuit bool
		headroom  float64
		dimmed    bool
		wantDim   *bool // nil means Dim() must not be called
	}{
		{"no circuit, no hems: untouched", nil, true, 0, false, nil},
		{"hems alone dims", lo.ToPtr(true), true, 0, false, lo.ToPtr(true)},
		{"circuit saturated dims without hems", nil, false, -1, false, lo.ToPtr(true)},
		{"circuit saturated, already dimmed: no call", nil, false, -1, true, nil},
		{"circuit frees up, no hems: undims", nil, false, 10, true, lo.ToPtr(false)},
		{"hems forces dim despite free circuit", lo.ToPtr(true), false, 10, false, lo.ToPtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			mockCharger := api.NewMockCharger(ctrl)
			mockDimmer := api.NewMockDimmer(ctrl)
			charger := &struct {
				api.Charger
				api.Dimmer
			}{Charger: mockCharger, Dimmer: mockDimmer}

			mockCharger.EXPECT().Enabled().Return(true, nil).AnyTimes()
			mockCharger.EXPECT().Enable(gomock.Any()).Return(nil).AnyTimes()
			mockCharger.EXPECT().Status().Return(api.StatusC, nil).AnyTimes()
			mockCharger.EXPECT().MaxCurrent(gomock.Any()).Return(nil).AnyTimes()

			lp := &Loadpoint{
				log:            util.NewLogger("foo"),
				bus:            evbus.New(),
				clock:          clock.New(),
				charger:        charger,
				chargeMeter:    newChargeMeter(&Null{}),
				chargeRater:    &Null{},
				chargeTimer:    &Null{},
				wakeUpTimer:    NewTimer(),
				minCurrent:     minA,
				maxCurrent:     maxA,
				phases:         1,
				solarShare:     1,
				status:         api.StatusC,
				mode:           api.ModeOff,
				alwaysCharge:   api.AlwaysChargeOff,
				enabled:        true,
				chargePower:    100,
				offeredCurrent: 6,
			}

			if !tc.noCircuit {
				circuit := api.NewMockCircuit(ctrl)
				circuit.EXPECT().ValidatePower(gomock.Any(), gomock.Any()).DoAndReturn(func(old, new float64) float64 {
					return min(new, old+tc.headroom)
				}).AnyTimes()
				circuit.EXPECT().ValidateCurrent(gomock.Any(), gomock.Any()).DoAndReturn(func(old, new float64) float64 {
					return min(new, old+tc.headroom)
				}).AnyTimes()
				lp.circuit = circuit
			}

			mockDimmer.EXPECT().Dimmed().Return(tc.dimmed, nil)
			if tc.wantDim != nil {
				mockDimmer.EXPECT().Dim(*tc.wantDim).Return(nil)
			}

			lp.Update(0, 0, nil, nil, false, false, 0, nil, nil, tc.hemsDim)

			ctrl.Finish()
		})
	}
}
