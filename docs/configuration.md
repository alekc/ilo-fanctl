# Configuration

[config.example.yaml](../config.example.yaml) is fully commented and is the
reference. This page is the summary; the example file is the authority.

## Shape

- `interval`: how often a cycle runs, default `30s`. A cycle costs roughly
  three seconds with six fans, so anything under `15s` leaves no idle time.
- `tools`: absolute paths to `ipmitool` and `smartctl`.
- `sensors`: named groups, each `kind: smart` (with `devices`) or `kind: ipmi`
  (with `match`, listing exact `ipmitool sdr type Temperature` names),
  aggregated by `max` or `mean`.
- `fans`: iLO fan indexes, **zero-based**, so index 0 is the fan iLO labels
  "Fan 1". Each belongs to one or more zones.
- `curves`: each maps one sensor group onto a demanded floor for its zones,
  piecewise linear, clamped at both ends rather than extrapolated.
- `safety`: floor bounds, hysteresis, ramp-down rate, the sensor-error policy,
  and critical thresholds.
- `ilo`: host, port, user, the auth source, the pinned host key, and the SSH
  algorithm lists. See [BMC access](../README.md#bmc-access-the-ilo-user-and-ssh-key).
- `listen`: the address the Prometheus exporter binds. See
  [Observability](observability.md).

Where several curves cover the same fan, **the highest demand wins**: the cpu
curve raises the same fans the drives curve does, and neither can hold the other
down. That is the answer to "what if the CPU is hot but the drives are cold".

## Validation

Everything is validated at startup and the process refuses to run on a config it
cannot fully make sense of. A typo'd key, a curve that demands less cooling as
temperature rises, a zone matching no fan, an IPMI sensor the BMC does not
report: all are startup errors, not runtime surprises.

Check a file without running anything against it:

```sh
ilo-fanctl -config /etc/ilo-fanctl/config.yaml -check
```

That validates and exits. It writes nothing and connects to nothing, so it is
safe to run against a production config from anywhere.

## Reloading

The config file is re-read while running. An edit takes effect within a few
seconds; `systemctl reload ilo-fanctl` applies it immediately. A file that fails
validation is rejected, the previous config keeps running, and
`ilo_fanctl_config_reloads_total{result="failure"}` increments, so a bad edit
cannot stop the cooling.

`listen` is the one exception and is applied at startup only, because rebinding
under a live scrape would drop it silently.

Fans removed from the config are wound down to `min_floor_pct` rather than left
where they were, and named in the log. A floor this tool set cannot be un-set,
only lowered, because the BMC has no way to report its own factory minimum.
