# Coalescing MachineConfig updates

MachineConfig updates can trigger a costly Machine Config Operator (MCO) rollout and node reboot. Autopilot therefore creates MachineConfigs immediately, but stages updates to an existing Autopilot-managed MachineConfig while all matching MachineConfigPools (MCPs) are stable.

When any matching MCP reports `Updating=True`, Autopilot applies the latest staged version. MCO can then include that change in the rollout already in progress. The first matching updating MCP wins. Staging is stored in the `virt-platform-autopilot-mc-staging` ConfigMap in the HyperConverged namespace, so an operator restart does not discard it. The ConfigMap is owned by HyperConverged, and entries are removed when their MachineConfig leaves Autopilot's active set.

## Pool matching and scope

A MachineConfig does not name a pool. Each MCP selects MachineConfigs using `spec.machineConfigSelector`, so one MachineConfig can match more than one pool. For example, a custom `infra` pool can also select `machineconfiguration.openshift.io/role: worker` MachineConfigs. Autopilot evaluates every matching pool and releases the staged update as soon as the first one is updating.

This prevents an update from waiting indefinitely for all matching pools to overlap. It does not change MCO fan-out: once a shared MachineConfig is updated, MCO can roll out every pool that selects it, including pools that were stable at release time.

The MachineConfig API being present while the MachineConfigPool API is absent is
an extreme compatibility corner case. In that case Autopilot has no rollout
signal to coalesce against, so it retains normal immediate MachineConfig
reconciliation rather than leaving updates staged indefinitely.

## Bypass staging

For a time-sensitive change, add this annotation to the live Autopilot-managed MachineConfig:

```yaml
metadata:
  annotations:
    platform.kubevirt.io/bypass-mcp-rollout-coalescing: "true"
```

Autopilot preserves this annotation and applies future updates immediately. Remove it to resume staging. Use it sparingly: it can start a new MCO rollout and reboot nodes.

## Observability

`kubevirt_autopilot_machineconfig_update_staged{machineconfig,pool}` is `1` for each matching MCP while an update is staged. `kubevirt_autopilot_machineconfig_update_staged_since_seconds` records when it was first staged. The **Autopilot / Asset Health** dashboard includes a **Staged MachineConfig Updates** table.
