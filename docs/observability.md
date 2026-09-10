# Observability

Prometheus exposition on `listen`, default `127.0.0.1:9873/metrics`. There is
nothing to enable: the exporter binds at startup and the same socket doubles as
the mutual exclusion that stops two processes driving the same BMC.

## Metrics

| Metric | Meaning |
| --- | --- |
| `ilo_fanctl_sensor_celsius{source,name}` | One physical sensor |
| `ilo_fanctl_sensor_group_celsius{group}` | Aggregated group value |
| `ilo_fanctl_sensor_group_blind{group}` | 1 while the group cannot be read |
| `ilo_fanctl_sensor_group_held{group}` | 1 while it is running on its last good value |
| `ilo_fanctl_sensor_errors_total{group}` | Collector failures, per group |
| `ilo_fanctl_curve_demand_percent{curve}` | What one curve is asking for |
| `ilo_fanctl_fan_applied_percent{fan,label}` | Floor this tool commanded |
| `ilo_fanctl_fan_observed_percent{fan,label}` | Speed the BMC reports |
| `ilo_fanctl_fan_writes_total` | Floor commands sent to the BMC |
| `ilo_fanctl_control_effective` | 1 when the floors are being honoured, NaN until known |
| `ilo_fanctl_readback_mismatch_total{fan}` | Commanded floor not reflected, ever |
| `ilo_fanctl_fan_mismatch{fan,label}` | 1 while that fan is below its floor now, NaN until it can be judged |
| `ilo_fanctl_blind_sensor_groups` | Groups running on a held or fallback value |
| `ilo_fanctl_critical_active` | 1 while a critical threshold is exceeded |
| `ilo_fanctl_ilo_connected` | 1 while the SSH session is up |
| `ilo_fanctl_config_reloads_total{result}` | Reload attempts |
| `ilo_fanctl_cycles_total`, `ilo_fanctl_cycle_errors_total{stage}` | Loop health |
| `ilo_fanctl_cycle_duration_seconds` | Loop latency |
| `ilo_fanctl_last_successful_cycle_timestamp_seconds` | Staleness |
| `ilo_fanctl_config_info{checksum}` | Which config is actually running |
| `ilo_fanctl_config_loaded_timestamp_seconds` | When that config was loaded |
| `ilo_fanctl_build_info{version}` | Which build is actually running |
| `ilo_fanctl_start_time_seconds` | Process start, for uptime |

A gauge here never holds a value that was not measured. When a sensor group goes
unreadable its per-sensor series are removed rather than frozen, and the group's
own value is removed as soon as the hold expires. A stale temperature that reads
as a cool component is the most dangerous thing this program could publish.

`fan_mismatch` is a verdict, not a measurement, and it is the one to read rather
than deriving your own from `fan_applied_percent` and `fan_observed_percent`. A
floor that went up seconds ago has not reached the fan yet, so those two gauges
genuinely disagree for a cycle without anything being wrong, and the daemon
judges against the floor that has been in effect for a full interval instead.
That floor is not exported, so the subtraction cannot be done correctly from
outside. `readback_mismatch_total` answers a different question: it counts
whether this ever happened, where the gauge says whether it is happening now.

There is a series for every configured fan from the first judged cycle
onwards, healthy or not, so the family being absent means the daemon predates
it rather than that nothing is wrong. That distinction is what lets a reader
tell an old daemon apart from a current one with no fan to name, which matters
because `control_effective` and these gauges are written a moment apart and a
scrape can land between them.

`build_info` and `config_info` are always 1, so the value carries nothing and
the label is the point. A rollout is then two queries rather than a round of
SSH, one for the shape of it and one for the stragglers:

```promql
# how many hosts on each version
count by (version) (ilo_fanctl_build_info)

# which hosts are not on the one being rolled out. Keep the whole series
# rather than aggregating: `instance` is the answer, and `by (version)`
# above deliberately throws it away.
ilo_fanctl_build_info{version!="v0.2.0"}
```

A build older than the release that fixed the SSH session teardown matters
more than most version drift, because that daemon stays `active (running)`
while writing into a dead pipe. See Detecting a reverted ilo4_unlock patch in
the README for the other failure that looks healthy from the outside.

## Alerts

```yaml
- alert: ILOFanControlIneffective
  expr: ilo_fanctl_control_effective == 0
  for: 5m
  annotations:
    summary: Fan floors are being written but the BMC is ignoring them

- alert: ILOFanCtlStalled
  expr: time() - ilo_fanctl_last_successful_cycle_timestamp_seconds > 300
  annotations:
    summary: No successful control cycle in five minutes
```

The first is the one that matters. It is how a reverted `ilo4_unlock` patch
surfaces, and that failure is otherwise completely silent. See
[Detecting a reverted ilo4_unlock patch](../README.md#detecting-a-reverted-ilo4_unlock-patch)
for why it is judged a full interval late, and why the metric starts at NaN
rather than 0.

Worth adding for a machine you care about:

```yaml
- alert: ILOFanCtlSensorGroupBlind
  expr: ilo_fanctl_sensor_group_blind == 1
  for: 10m
  annotations:
    summary: "Sensor group {{ $labels.group }} has been unreadable for ten minutes"

- alert: ILOFanCtlCritical
  expr: ilo_fanctl_critical_active == 1
  annotations:
    summary: A critical temperature threshold is being exceeded
```

## Dashboard

`ilo_fanctl_fan_applied_percent` against `ilo_fanctl_fan_observed_percent` on
one graph is the panel that tells you whether the thing is working. Observed
above applied is normal and means iLO's own curve is asking for more. Observed
below applied is the failure this tool exists to detect.

Beside it, `ilo_fanctl_sensor_group_celsius` for every group on one axis gives
the same picture as the trends pane in the terminal display, and
`ilo_fanctl_curve_demand_percent` shows which curve is currently driving.

## Logs

Structured `key=value` text on stderr, so `journalctl -u ilo-fanctl` is the
reader. `-log-level` takes `debug`, `info`, `warn` or `error` and defaults to
`info`.

Every floor written is logged at info with the fan index, the label, the
percentage and the raw PWM value:

```
fan floor set fan=3 label="Fan 4" percent=22 raw=56 critical=false
```

so the journal is a complete record of what this tool commanded and when. A
read-back mismatch is logged at error and names the fan, and a rejected config
reload is logged at error with the validation failure while the previous config
keeps running.
