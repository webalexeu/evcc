package core

import (
	"testing"

	evbus "github.com/asaskevich/EventBus"
	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/samber/lo"
	"go.uber.org/mock/gomock"
)

// A manual API override lets a Dimmer-capable charger be forced to dim, independent
// of any HEMS/§14a signal, and released again. An API request to stop forcing dim
// never overrides an active HEMS/§14a signal, since that's a compliance constraint,
// not a preference.
func TestLoadpointUpdateDim(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hemsDim  *bool
		forceDim *bool
		dimmed   bool
		wantDim  *bool // nil means Dim() must not be called
	}{
		{"no hems, no api: untouched", nil, nil, false, nil},
		{"hems alone dims", lo.ToPtr(true), nil, false, lo.ToPtr(true)},
		{"api forces dim alone", nil, lo.ToPtr(true), false, lo.ToPtr(true)},
		{"api release undoes a stuck forced dim", nil, lo.ToPtr(false), true, lo.ToPtr(false)},
		{"hems overrides an api dim-off attempt", lo.ToPtr(true), lo.ToPtr(false), false, lo.ToPtr(true)},
		{"api dim-on overrides hems saying no limit", lo.ToPtr(false), lo.ToPtr(true), false, lo.ToPtr(true)},
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
				log:          util.NewLogger("foo"),
				bus:          evbus.New(),
				clock:        clock.New(),
				charger:      charger,
				chargeMeter:  newChargeMeter(&Null{}),
				chargeRater:  &Null{},
				chargeTimer:  &Null{},
				wakeUpTimer:  NewTimer(),
				minCurrent:   minA,
				maxCurrent:   maxA,
				phases:       1,
				solarShare:   1,
				status:       api.StatusC,
				mode:         api.ModeOff,
				alwaysCharge: api.AlwaysChargeOff,
				enabled:      true,
				forceDim:     tc.forceDim,
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
