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
