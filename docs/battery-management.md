# Battery Management — Technical Reference

This document describes the full battery management stack implemented in this fork of evcc, covering both the upstream features and the custom extensions added for multi-battery RS485 control (Marstek Venus E Gen 3).

---

## 1. Battery Modes

The site maintains a `batteryMode` that is sent to each battery via `SetBatteryMode()` (the `BatteryController` API). Three modes are defined:

| Mode | Value | Effect |
|------|-------|--------|
| Normal | 1 | Battery uses its own internal algorithm (anti-feed / self-consumption) |
| Hold | 2 | RS485 control active; charge/discharge direction held at Stop (0W) |
| Charge | 3 | Force-charge at rated power until maxSoc is reached |

**Hold** is used as the "RS485 enabled, waiting for per-tick power commands" state. Without Hold, the inverter resets to Normal between ticks and ignores power commands.

---

## 2. Battery Mode Selection (`requiredBatteryMode`)

Each tick the system evaluates which mode to apply, in priority order:

1. **External control** (`batteryModeExternal`) — set via MQTT/HTTP, overrides everything until cleared
2. **External reset** — when external mode is cleared, forces Normal once to hand control back
3. **Grid charge active** — forces Charge mode (charges from grid at cheap tariff)
4. **Discharge control active** — forces Hold (prevents discharge during fast/planned EV charging)
5. **Solar control active** — keeps Hold so RS485 commands own every tick
6. **Modified mode cleanup** — returns to Normal if mode was previously modified

---

## 3. Solar Control (`batterySolarControl`)

When enabled, the site drives `SetBatteryChargePower` / `SetBatteryDischargePower` on each battery every tick via the `BatteryPowerController` API. This gives watt-level control instead of binary charge/discharge.

### 3.1 Surplus Calculation

Two formulas are used depending on battery SoC relative to `prioritySoc`:

**Normal mode** (`soc >= prioritySoc`):
```
surplus = -sitePower
```

**Priority mode** (`soc < prioritySoc`):
```
surplus = -(batteryPower + gridPower)
         = pvPower - housePower - EV
```

The priority-mode formula is derived from the energy balance identity and is sign-convention agnostic — it works correctly regardless of whether the battery meter reports positive or negative values for charging (handles both standard evcc convention and inverted conventions like Marstek register 30006).

### 3.2 Control Decision (`threshold`)

The outer switch uses a combined threshold:
```
threshold = standbyPower(10W) + batteryControlDeadBand
```

| Condition | Action |
|-----------|--------|
| `surplus > threshold` | Charge batteries from solar surplus |
| `sitePower > threshold` | Discharge batteries to cover home deficit |
| otherwise | Stop all (balanced / idle) |

---

## 4. Dead Band (`batteryControlDeadBand`)

An additional threshold on top of `standbyPower` (10W) before the system starts or continues charge/discharge. Prevents the control loop from reacting to small measurement noise around the zero-grid setpoint.

- **Default**: 0W (backward-compatible; effective threshold = 10W)
- **Recommended**: 50–100W for installations with noisy grid meters
- **API**: `POST /batterycontroldeadband/{value}` | MQTT `batteryControlDeadBand`

---

## 5. Tiered Activation (`computeTier`)

Uses the minimum number of batteries needed to handle the target power without each unit operating at a fraction of its rated capacity. Concentrating load avoids sub-threshold commands that inverters silently ignore and keeps each unit at a more efficient operating point.

### Tier boundaries (example: 3 × Marstek 2000W charge / 800W discharge)

**Charging:**
| Tier | Batteries active | Surplus range |
|------|-----------------|--------------|
| 1 | 1 | 0 – 2000W |
| 2 | 2 | 2000 – 4000W |
| 3 | 3 | > 4000W |

**Discharging:**
| Tier | Batteries active | Deficit range |
|------|-----------------|--------------|
| 1 | 1 | 0 – 800W |
| 2 | 2 | 800 – 1600W |
| 3 | 3 | > 1600W |

### Hysteresis (15% dead band)

To prevent rapid tier switching when power hovers near a boundary:
- **Switch up**: only when target > current-tier capacity × 1.15
- **Switch down**: only when target < previous-tier capacity × 0.85
- **Large jump (> 1 tier)**: responds immediately without dead band

Tier state (`batteryChargeTier`, `batteryDischargeTier`) is persisted between ticks and reset to 0 on startup.

---

## 6. Battery Selection within a Tier

Within the active tier, batteries are selected by SoC to naturally balance the pack over time:

- **Charging**: select the N **lowest-SoC** batteries (fill the most depleted first)
- **Discharging**: select the N **highest-SoC** batteries (drain the fullest first)

Non-selected batteries receive a stop command (both `SetBatteryChargePower(0)` and `SetBatteryDischargePower(0)`).

### Fallback (no `BatteryPowerLimiter`)

For batteries without power limits, the original flat minimum threshold is used: if the per-battery share would be below 50W (the minimum Marstek acts on), the full surplus is concentrated on the single best candidate.

---

## 7. Sticky Selection (SoC Hysteresis)

Re-selecting batteries purely by SoC on every tick causes ping-pong oscillation when batteries have similar SoC (e.g., 70% vs 69%). Each switch generates unnecessary Modbus writes and inverter ramp-up cycles.

### How it works

The active battery set is persisted between ticks (`batteryChargeActive`, `batteryDischargeActive`). On each tick:

1. If the stored set is still valid (same size, all names still in pool): keep it
2. Check if any non-active battery is **more than 3% better** than the worst battery in the active set:
   - Charging: candidate SoC < worst active SoC − 3%
   - Discharging: candidate SoC > worst active SoC + 3%
3. If yes: swap that one battery in (one swap per tick maximum)
4. If no: keep the current set unchanged

**Effect**: a battery holds its role until another unit is clearly better — no flipping due to integer SoC quantisation noise.

### Reset conditions
- Tier size changes (a battery joins or leaves the pool via minSoc/maxSoc)
- `batterySolarControl` toggles off/on

---

## 8. Charge Tapering

Linearly reduces charge power in the last 10% of SoC before `maxSoc`. Mimics the CC/CV charging profile that protects lithium cells from stress near full charge.

```
taperFactor = (maxSoc - currentSoc) / chargeTaperRange   (clamped to minimum 0.10)
chargePower = requestedPower × taperFactor
```

- **Taper range**: 5% SoC below maxSoc
- **Minimum factor**: 25% of requested power (never fully stopped by taper)
- **Per-battery**: applied individually using each battery's `BatterySocLimiter.GetSocLimits()`
- Applied after the hard-cap from `BatteryPowerLimiter`
- **Skipped during LFP calibration**: when `batteryCalibrationCharge` is active, tapering is bypassed entirely so batteries charge at full surplus power all the way to 100%

---

## 9. Priority SoC (`prioritySoc`)

When battery SoC is below this threshold:

- The surplus formula switches to the energy-balance variant `-(batPow + gridPow)` for sign-convention robustness
- Discharge is **not** blocked (this is a charging-priority concept only, not a discharge gate)
- The battery gets first claim on solar surplus before EV charging is allowed (handled upstream in `sitePower` calculation)

---

## 10. Buffer SoC (`bufferSoc` / `bufferStartSoc`)

Controls battery-supported EV charging:

- **`bufferSoc`**: when battery SoC is above this level, battery power is included in the available budget for EV charging even without solar surplus
- **`bufferStartSoc`**: EV charging from battery only starts when SoC exceeds this level (hysteresis to prevent immediately draining a partially-charged battery)

---

## 11. Discharge Control (`batteryDischargeControl`)

When enabled, prevents battery discharge during:
- **Fast/planned charging**: car is connected (StatusB+) and fast charging is active — StatusC not required so phase-negotiation transitions don't momentarily re-enable discharge
- **Smart cost active**: car is actually charging (StatusC) and the current tariff rate is below the smart cost limit

---

## 12. Grid Charge (`batteryGridChargeLimit`)

When the current grid tariff price is at or below this limit, forces Charge mode (charges battery from grid at rated power). Useful for time-of-use tariffs.

---

## 13. MaxSoc Enforcement

When Charge mode is active, `applyBatteryMode` checks each battery's SoC against its `maxSoc` limit (via `BatterySocLimiter`). If any battery has reached `maxSoc`, the mode is switched to Hold to stop charging that unit.

In solar control mode, the tiered selection also filters out batteries that have reached `maxSoc` (moved to the `full` list and stopped).

---

## 14. MinSoc Enforcement

In the discharge case, batteries whose SoC is at or below their hardware `minSoc` (from `BatterySocLimiter`) are moved to the `empty` list and stopped. They are excluded from the active discharge tier.

---

## 15. Command Ordering & Latency

The control loop minimises the time between the grid measurement and the command reaching the active battery, and avoids gaps during battery handoffs.

### Safe swap ordering

When sticky selection swaps one battery for another, the command is sent to the **incoming** battery first and its Modbus result is checked:

1. Incoming battery receives its charge/discharge command
2. **Failure** (Modbus error): outgoing battery keeps running and receives the share as a one-tick fallback; the next tick re-evaluates selection normally
3. **Success** (Modbus ACK): the outgoing battery's fate depends on direction:
   - **Discharge swap**: the outgoing battery is *not* stopped this tick — a Modbus ACK only confirms the register write, while the incoming inverter still needs seconds to ramp up. The outgoing battery keeps covering the load for one more tick (stopped on the next tick via the regular non-selected path). The overlap briefly exports to grid (safe); stopping immediately would import during the ramp.
   - **Charge swap**: the outgoing battery is stopped immediately (deferred). Here the asymmetry reverses: a ramp gap merely exports surplus (safe), while an overlap would charge both batteries and import from grid (unsafe).

### Per-cycle SoC cache

All battery SoC values are read **once per control cycle** into a cache. Selection loops and sort comparators perform map lookups instead of issuing repeated Modbus reads (previously the sort comparator re-read the same battery's SoC on every comparison).

### Deferred stops

Stop commands for non-selected batteries (full, empty, outside the tier) are queued and executed **after** the active batteries have received their power commands. The Modbus writes for inactive units stay off the critical path, so the active battery reacts to a load change one or more seconds sooner. During a tier shrink this causes a brief overlap (remaining battery ramps up before the other stops) which errs toward grid export — the safe direction.

### Failed meter read guard

When the grid meter read fails (`sitePower` cannot be computed), the solar power control **skips the tick entirely** instead of acting on a zero value — a zero would be mistaken for "balanced" and stop all batteries for one tick, dropping the load onto the grid. Batteries hold their last setpoints until the next successful read. Battery *mode* handling still runs on such ticks.

### Redundant stop suppression

A battery that is already stopped is not re-stopped every tick. `stopAll` tracks ticks-since-last-stop per battery (`batteryStopped`) and skips the Modbus writes while the battery remains stopped, re-sending the stop every `stopRefreshTicks` (10) ticks as a watchdog heartbeat so RS485 control stays alive. Any active power command (including the swap fallback) clears the tracking so the next stop is always sent immediately; a failed stop is retried on the next tick. This frees roughly two writes per inactive battery per tick of Modbus bus time.

---

## 16. Battery Fast Loop

A dedicated 1s loop (`core/site_battery_fast.go`) closes the reaction gap between main loop ticks. The split keeps all intelligence in the main loop:

| | Main loop | Fast loop |
|---|---|---|
| Direction (charge/discharge/idle) | ✔ decides | never changes |
| Tiering / sticky / swaps | ✔ | — |
| Stop commands / mode writes | ✔ | — |
| SoC reads / taper | ✔ | — |
| Power commands | only on activation, direction change, swap | ✔ owns steady state, every 500ms tick |

**Single-writer principle**: while the fast loop is active (grid meter present), the main loop does **not** re-command power to batteries that are already active in the same direction — its meter snapshot suffers the same sampling skew as any other reading, and re-commanding from it injects phantom values that the fast loop then has to correct. The main loop issues power commands only when a battery joins the active set (was stopped), on direction change, or during swap handling (where the Modbus ACK check drives the safe-handoff logic). A 10s heartbeat in the fast loop re-sends the current setpoints when no write happened, keeping the inverters' RS485 watchdog alive.

**Contract**: the main loop publishes a `batteryControlPlan` snapshot (direction, active entries with effective power caps, EV-excluded power, commanded total) at the end of every `applyBatterySolarPower` run. Both sides synchronize on `batteryPlanMu`, which also serializes the entire main-loop battery section against fast-loop ticks — no stale-plan write can re-activate a stopped battery.

**Tick structure** (500ms period): the grid register is read first; if its value is identical to the previous tick (stale register — the meter refreshes every ~2-3s), the tick ends after that single cheap read. Battery power reads and the correction only run on fresh grid samples, so the fast tick rate costs almost nothing on the Modbus bus. Power writes to multiple active batteries go out **in parallel** (each battery has its own connection), so multi-battery tiers command as fast as a single unit.

**Correction math** (grid meter read + one power read per active battery per fresh tick):
- discharge: `target = batteryMeasured + gridPower + gridOffset − evExcluded`
- charge: `target = −batteryMeasured − (gridPower + gridOffset)`
- `gridOffset` is the grid setpoint the main loop steered toward (residualPower, or 0 below prioritySoc)
- The target is an **absolute energy balance from measurements**, not an increment on the commanded value. This is essential: during inverter ramps the commanded power is not yet delivered, and integrating the still-visible grid error against the commanded total double-counts it and produces full-scale oscillation (observed in practice). The measured form is ramp-state invariant.
- applied at full gain (1.0) for one-tick reaction; clamped to `[0, cap]` per battery; corrections < 10W are skipped
- **Meter consistency guard**, two rules evaluated per tick:
  1. *Stale grid register*: a grid reading identical to the previous tick carries no new information (the meter refreshes slower than 1s) — the tick is skipped silently. Corrections only happen on fresh grid samples.
  2. *Sampling skew*: with constant load, Δgrid + Δbattery ≈ 0 between ticks. When |Δbattery| > 100W while |Δgrid + Δbattery| > 100W, the registers are out of sync and the energy balance would double-count — the tick is skipped until they align.
  Genuine load steps (Δbattery ≈ 0, fresh grid) are never skipped, preserving one-tick reactivity. The guard history is seeded from the main loop's readings at plan creation, so the first fast tick after each main tick is guarded too.

**Safety rules**:
- Direction flips are never done by the fast loop — corrections clamp at 0 and wait for the main loop
- Plan stays **idle on swap ticks** (ramp/overlap makes the commanded-power proxy unreliable) — fast loop pauses for one main tick
- Plan older than 30s (main loop stalled) → fast loop parks
- Failed grid read → skip tick

---

## Configuration Summary

| Setting | API | MQTT | Default | Description |
|---------|-----|------|---------|-------------|
| `batterySolarControl` | POST `/batterysolarcontrol/{bool}` | `batterySolarControl` | false | Enable watt-level solar charge/discharge control |
| `batteryControlDeadBand` | POST `/batterycontroldeadband/{W}` | `batteryControlDeadBand` | 0 | Extra dead band before starting charge/discharge |
| `batteryDischargeControl` | POST `/batterydischargecontrol/{bool}` | `batteryDischargeControl` | false | Prevent discharge during fast/smart charging |
| `batteryGridChargeLimit` | POST `/batterygridchargelimit/{price}` | `batteryGridChargeLimit` | null | Charge from grid when tariff ≤ this price |
| `batteryMode` | POST `/batterymode/{mode}` | `batteryMode` | normal | External mode override (normal/hold/charge) |
| `prioritySoc` | POST `/prioritysoc/{%}` | `prioritySoc` | 0 | Battery charging priority threshold |
| `bufferSoc` | POST `/buffersoc/{%}` | `bufferSoc` | 0 | SoC above which battery supports EV charging |
| `bufferStartSoc` | POST `/bufferstartsoc/{%}` | `bufferStartSoc` | 0 | SoC threshold to start battery-supported charging |

### Internal constants (not configurable at runtime)

| Constant | Value | Description |
|----------|-------|-------------|
| `standbyPower` | 10W | Minimum power considered non-zero |
| `chargeTaperRange` | 5% SoC | SoC band in which charge is tapered |
| `chargeMinFactor` | 25% | Minimum taper factor at maxSoc |
| Tier hysteresis | 15% | Dead band around tier-switch boundaries |
| Sticky SoC threshold | 3% | Minimum SoC difference to swap active battery |
| `stopRefreshTicks` | 10 ticks | Heartbeat interval for re-sending stop to stopped batteries |
