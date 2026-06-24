<template>
	<div class="container px-4 safe-area-inset">
		<TopHeader :title="$t('batterySettings.modalTitle')" />
		<div class="row">
			<main class="col-12">
				<template v-if="batteryAvailable">
					<h3 class="fw-normal my-4">
						{{ $t("batterySettings.usageTab") }}
					</h3>
					<BatteryUsageSettings style="max-width: 950px" v-bind="batteryUsageProps" />
				<template v-if="solarControlPossible">
						<hr class="my-5" />
						<h3 class="fw-normal my-4 mt-5">
							{{ $t("batterySettings.batteryControlTab") }}
						</h3>
						<div class="form-check form-switch">
							<input
								id="batterySolarControl"
								:checked="state.batterySolarControl"
								class="form-check-input"
								type="checkbox"
								role="switch"
								@change="changeSolarControl"
							/>
							<label class="form-check-label" for="batterySolarControl">
								{{ $t("batterySettings.batteryControl") }}
							</label>
						</div>
					</template>
					<template v-if="gridChargeVisible">
						<hr class="my-5" />
						<h3 class="fw-normal my-4 mt-5">
							{{ $t("batterySettings.gridChargeTab") }}
						</h3>
						<SmartCostLimit v-bind="smartCostLimitProps" />
					</template>
				</template>
				<p v-else class="my-4 text-muted">
					{{ $t("batterySettings.noBattery") }}
				</p>
			</main>
		</div>
	</div>
</template>

<script lang="ts">
import { defineComponent } from "vue";
import Header from "../components/Top/Header.vue";
import BatteryUsageSettings from "../components/Battery/BatteryUsageSettings.vue";
import SmartCostLimit from "../components/Tariff/SmartCostLimit.vue";
import store from "../store";
import settings from "../settings";
import collector from "../mixins/collector";
import api from "../api";
import { SMART_COST_TYPE } from "../types/evcc";

export default defineComponent({
	name: "Battery",
	components: {
		TopHeader: Header,
		BatteryUsageSettings,
		SmartCostLimit,
	},
	mixins: [collector],
	head() {
		return { title: this.$t("batterySettings.modalTitle") };
	},
	computed: {
		state() {
			return store.state;
		},
		batteryAvailable() {
			return (this.state.battery?.devices?.length ?? 0) > 0;
		},
		batteryUsageProps() {
			return this.collectProps(BatteryUsageSettings, this.state);
		},
		solarControlPossible() {
			const devices = this.state.battery?.devices ?? [];
			return devices.some(({ controllable }) => controllable);
		},
		gridChargePossible() {
			const devices = this.state.battery?.devices ?? [];
			return (
				devices.some(({ controllable }) => controllable) && this.state.smartCostAvailable
			);
		},
		gridChargeLimit() {
			return this.state.batteryGridChargeLimit ?? null;
		},
		gridChargeVisible() {
			return this.gridChargePossible || this.gridChargeLimit !== null;
		},
		gridChargeTariff() {
			const { forecast, smartCostType } = this.state;
			return smartCostType === SMART_COST_TYPE.CO2 ? forecast?.co2 : forecast?.grid;
		},
		smartCostLimitProps() {
			return {
				currentLimit: this.gridChargeLimit,
				lastLimit: settings.lastBatterySmartCostLimit,
				smartCostType: this.state.smartCostType,
				currency: this.state.currency,
				tariff: this.gridChargeTariff,
				possible: this.gridChargePossible,
			};
		},
	},
	methods: {
		async changeSolarControl(e: Event) {
			try {
				await api.post(
					`batterysolarcontrol/${(e.target as HTMLInputElement).checked ? "true" : "false"}`
				);
			} catch (err) {
				console.error(err);
			}
		},
	},
});
</script>
