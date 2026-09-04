<template>
	<div class="container px-4 safe-area-inset">
		<TopHeader :title="$t('batterySettings.modalTitle')" />
		<div class="row">
			<main class="col-12">
				<template v-if="batteryAvailable">
					<BatteryStatusCards
						class="mb-4 box-pull-out"
						:battery="state.battery"
						:battery-mode="state.batteryMode"
					/>

					<BatteryHistoryCard
						class="mb-4 box-pull-out"
						:batteries="chartBatteries"
						:now="now"
						:kwh-available="kWhAvailable"
						@range-start="onRangeStart"
					/>

					<BatteryConfigCard
						class="mb-4 box-pull-out"
						:buffer-soc="state.bufferSoc"
						:priority-soc="state.prioritySoc"
						:buffer-start-soc="state.bufferStartSoc"
						:battery-discharge-control="state.batteryDischargeControl"
						:battery-grid-discharge="state.batteryGridDischarge"
						:battery="state.battery"
						:experimental="state.experimental"
					/>

					<Card
						v-if="solarControlPossible"
						class="mb-4 box-pull-out"
						:title="$t('batterySettings.batteryControlTab')"
					>
						<div class="form-check form-switch mb-3">
							<input
								id="batteryExpSolarControl"
								:checked="state.batterySolarControl"
								class="form-check-input"
								type="checkbox"
								role="switch"
								@change="changeSolarControl"
							/>
							<label class="form-check-label" for="batteryExpSolarControl">
								{{ $t("batterySettings.batteryControl") }}
							</label>
						</div>
						<template v-if="state.batterySolarControl">
							<div class="mb-1 small text-muted fw-semibold text-uppercase">
								{{ $t("batterySettings.batteryControlModeTab") }}
							</div>
							<div class="d-flex gap-4 mb-3">
								<div class="form-check">
									<input
										id="batteryExpSolarModePerBattery"
										:checked="!state.batterySolarPool"
										class="form-check-input"
										type="radio"
										name="batteryExpSolarMode"
										@change="changePool(false)"
									/>
									<label
										class="form-check-label"
										for="batteryExpSolarModePerBattery"
									>
										{{ $t("batterySettings.batteryControlModePerBattery") }}
									</label>
								</div>
								<div class="form-check">
									<input
										id="batteryExpSolarModePool"
										:checked="state.batterySolarPool"
										class="form-check-input"
										type="radio"
										name="batteryExpSolarMode"
										@change="changePool(true)"
									/>
									<label class="form-check-label" for="batteryExpSolarModePool">
										{{ $t("batterySettings.batteryControlModePool") }}
									</label>
								</div>
							</div>
							<p class="text-muted small mb-3">
								{{
									state.batterySolarPool
										? $t("batterySettings.batteryControlModePoolDesc")
										: $t("batterySettings.batteryControlModePerBatteryDesc")
								}}
							</p>
							<template v-if="!state.batterySolarPool">
								<div class="form-check form-switch mb-3">
									<input
										id="batteryExpSolarTiering"
										:checked="state.batterySolarTiering"
										class="form-check-input"
										type="checkbox"
										role="switch"
										@change="changeTiering"
									/>
									<label class="form-check-label" for="batteryExpSolarTiering">
										{{ $t("batterySettings.tieringLabel") }}
									</label>
								</div>
							</template>
							<hr class="my-3" />
							<div class="fw-bold mb-2">
								{{ $t("batterySettings.taperingTab") }}
							</div>
							<div class="form-check form-switch">
								<input
									id="batteryExpSolarTapering"
									:checked="state.batterySolarTapering"
									class="form-check-input"
									type="checkbox"
									role="switch"
									@change="changeTapering"
								/>
								<label class="form-check-label" for="batteryExpSolarTapering">
									{{ $t("batterySettings.taperingLabel") }}
								</label>
							</div>
							<hr class="my-3" />
							<div class="fw-bold mb-2">
								{{ $t("batterySettings.calibrationTab") }}
							</div>
							<div class="form-check form-switch">
								<input
									id="batteryExpCalibrationCharge"
									:checked="state.batteryCalibrationCharge"
									class="form-check-input"
									type="checkbox"
									role="switch"
									@change="changeCalibrationCharge"
								/>
								<label class="form-check-label" for="batteryExpCalibrationCharge">
									{{ $t("batterySettings.calibrationLabel") }}
								</label>
							</div>
						</template>
					</Card>

					<Card
						v-if="gridChargeVisible"
						class="box-pull-out"
						:title="$t('batterySettings.gridChargeTab')"
					>
						<SmartCostLimit v-bind="smartCostLimitProps" />
					</Card>

					<Card
						v-if="gridDischargeVisible"
						class="box-pull-out mt-4"
						:title="$t('batterySettings.gridDischargeTab')"
						data-testid="battery-grid-discharge-limit"
					>
						<SmartFeedInPriority v-bind="smartFeedInPriorityProps" />
					</Card>
				</template>
				<p v-else class="my-4 text-muted">{{ $t("batterySettings.noBattery") }}</p>
			</main>
		</div>
	</div>
</template>

<script lang="ts">
import { defineComponent } from "vue";
import store from "@/store";
import settings from "@/settings";
import api from "@/api";
import { SMART_COST_TYPE, CURRENCY, type BatteryMeter } from "@/types/evcc";
import Header from "../components/Top/Header.vue";
import Card from "../components/Helper/Card.vue";
import SmartCostLimit from "../components/Tariff/SmartCostLimit.vue";
import SmartFeedInPriority from "../components/Tariff/SmartFeedInPriority.vue";
import BatteryStatusCards from "../components/Battery/BatteryStatusCards.vue";
import BatteryConfigCard from "../components/Battery/BatteryConfigCard.vue";
import BatteryHistoryCard from "../components/Battery/BatteryHistoryCard.vue";
import {
	historyToSeries,
	forecastToSeries,
	buildChartBatteries,
} from "../components/Battery/history";
import type { BatteryHistorySeries, BatterySeries } from "../components/Battery/types";

const CHUNK_MS = 48 * 3600 * 1000; // 48h load/grow step

export default defineComponent({
	name: "Battery",
	components: {
		TopHeader: Header,
		Card,
		SmartCostLimit,
		SmartFeedInPriority,
		BatteryStatusCards,
		BatteryConfigCard,
		BatteryHistoryCard,
	},
	head() {
		return { title: this.$t("batterySettings.modalTitle") };
	},
	data() {
		const now = new Date();
		return {
			now,
			rangeStart: new Date(now.getTime() - CHUNK_MS), // earliest time the chart currently shows
			loadedFrom: null as Date | null, // earliest time we have history for
			loadedTo: null as Date | null, // latest time we have history for
			loading: false,
			rawSeries: [] as BatteryHistorySeries[],
		};
	},
	computed: {
		state() {
			return store.state;
		},
		historyUpdated(): string | undefined {
			return store.state.historyUpdated;
		},
		devices(): BatteryMeter[] {
			return this.state.battery?.devices ?? [];
		},
		batteryAvailable(): boolean {
			return this.devices.length > 0;
		},
		evopt() {
			return this.state.evopt;
		},
		kWhAvailable(): boolean {
			return this.batteryAvailable && this.devices.every((d) => d.capacity > 0);
		},
		// earliest history we want loaded: a chunk before the visible start (kept as a buffer),
		// never later than what we already loaded so the window only grows
		requestedFrom(): Date {
			const wanted = new Date(this.rangeStart.getTime() - CHUNK_MS);
			return this.loadedFrom && this.loadedFrom < wanted ? this.loadedFrom : wanted;
		},
		chartBatteries(): BatterySeries[] {
			return buildChartBatteries(
				this.devices,
				historyToSeries(this.rawSeries),
				forecastToSeries(this.evopt, this.now.getTime())
			);
		},
		batteryControllable(): boolean {
			return this.devices.some(({ controllable }) => controllable);
		},
		solarControlPossible(): boolean {
			return this.batteryControllable;
		},
		gridChargePossible(): boolean {
			return this.batteryControllable && !!this.state.smartCostAvailable;
		},
		gridChargeLimit(): number | null {
			return this.state.batteryGridChargeLimit ?? null;
		},
		gridChargeVisible(): boolean {
			return this.gridChargePossible || this.gridChargeLimit !== null;
		},
		gridChargeTariff() {
			const { co2, grid } = store.uiForecast.value;
			return this.state.smartCostType === SMART_COST_TYPE.CO2 ? co2 : grid;
		},
		smartCostLimitProps() {
			return {
				currentLimit: this.gridChargeLimit,
				lastLimit: settings.lastBatterySmartCostLimit,
				smartCostType: this.state.smartCostType,
				currency: this.state.currency || CURRENCY.EUR,
				tariff: this.gridChargeTariff,
				possible: this.gridChargePossible,
			};
		},
		gridDischargeLimit(): number | null {
			return this.state.batteryGridDischargeLimit ?? null;
		},
		// needs a dynamic feed-in tariff, the grid price is irrelevant here
		gridDischargePossible(): boolean {
			return this.batteryControllable && !!this.state.smartFeedInPriorityAvailable;
		},
		// requires opt-in; a set limit stays visible so it can be removed
		gridDischargeVisible(): boolean {
			return (
				!!this.state.batteryGridDischarge &&
				(this.gridDischargePossible || this.gridDischargeLimit !== null)
			);
		},
		smartFeedInPriorityProps() {
			return {
				currentLimit: this.gridDischargeLimit,
				lastLimit: settings.lastBatteryGridDischargeLimit,
				currency: this.state.currency || CURRENCY.EUR,
				tariff: store.uiForecast.value.feedin,
				possible: this.gridDischargePossible,
			};
		},
	},
	watch: {
		// fetchHistory reloads only when the needed range is not already covered
		requestedFrom: {
			handler() {
				this.fetchHistory();
			},
			immediate: true,
		},
		now() {
			this.fetchHistory();
		},
		// bumped on each metrics persist; advance clock to reload recent history
		historyUpdated() {
			this.now = new Date();
		},
	},
	methods: {
		onRangeStart(start: Date) {
			this.rangeStart = start;
		},
		async changeSolarControl(e: Event) {
			try {
				await api.post(
					`batterysolarcontrol/${(e.target as HTMLInputElement).checked ? "true" : "false"}`
				);
			} catch (err) {
				console.error(err);
			}
		},
		async changeCalibrationCharge(e: Event) {
			try {
				await api.post(
					`batterycalibrationcharge/${(e.target as HTMLInputElement).checked ? "true" : "false"}`
				);
			} catch (err) {
				console.error(err);
			}
		},
		async changePool(value: boolean) {
			try {
				await api.post(`batterysolarpool/${value ? "true" : "false"}`);
			} catch (err) {
				console.error(err);
			}
		},
		async changeTiering(e: Event) {
			try {
				await api.post(
					`batterysolartiering/${(e.target as HTMLInputElement).checked ? "true" : "false"}`
				);
			} catch (err) {
				console.error(err);
			}
		},
		async changeTapering(e: Event) {
			try {
				await api.post(
					`batterysolartapering/${(e.target as HTMLInputElement).checked ? "true" : "false"}`
				);
			} catch (err) {
				console.error(err);
			}
		},
		async fetchHistory() {
			if (this.loading || !this.batteryAvailable) return;
			const from = this.requestedFrom;
			const to = this.now;
			const backLoaded = this.loadedFrom !== null && this.loadedFrom <= from;
			const frontLoaded = this.loadedTo !== null && this.loadedTo >= to;
			if (backLoaded && frontLoaded) return;
			this.loading = true;
			try {
				const res = await api.get("history/energy", {
					params: {
						from: from.toISOString(),
						to: to.toISOString(),
						aggregate: "15m",
						grouped: false,
						group: "battery",
					},
				});
				this.rawSeries = res.data || [];
				this.loadedFrom = from;
				this.loadedTo = to;
			} catch (e) {
				console.error("failed to load battery history", e);
			} finally {
				this.loading = false;
			}
		},
	},
});
</script>
