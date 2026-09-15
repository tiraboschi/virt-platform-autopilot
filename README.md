# virt-platform-autopilot

Production-ready OpenShift Virtualization with zero manual configuration. The autopilot detects your hardware, applies best practices, and configures the platform automatically. **Convention over Configuration** for enterprise virtualization.

## The Problem

Traditional virtualization platform setup requires:
- Manual NUMA topology configuration
- GPU/PCI device passthrough setup
- High-performance networking configuration
- Load-aware workload placement policies
- Auto-remediation for node failures
- Coordinating multiple operators, MachineConfigs, and KubeletConfigs

**That's weeks of work requiring deep Kubernetes expertise.**

## The Solution

The autopilot applies opinionated best practices and manages your platform automatically:
* **Production defaults** - Optimized HCO, platform configurations, kubelet settings
* **Operational excellence** - Auto-remediation, intelligent workload placement
* **Zero API surface** - No new CRDs, no status fields to check
* **Silent operation** - Alerts only when user intervention is required
* **GitOps-friendly** - Declarative control via annotations, fully customizable
* **Convention over Configuration** - Opinionated entry point, flexible when needed

**Install once. Run VMs. Customize via GitOps when needed.**

## Features

<!-- BEGIN FEATURE STATUS -->
| Feature | Description | Maturity | Install | Dependencies |
|---------|-------------|----------|---------|--------------|
| Incident Detection | OpenShift Incident Detection groups related cluster alerts into a single, cohesive timeline to eliminate alert fatigue and speed up troubleshooting. | GA | always | Cluster Observability Operator |
| Load-Aware Descheduler | Load-aware VM balancing based on CPU/memory utilization and pressure stall metrics | GA | always | Kube Descheduler Operator |
| Observability Enhancements | Enhanced observability with additional Prometheus alerting rules and advanced Perses dashboards in the OpenShift console | GA | always | Cluster Observability Operator |
| Swap Enablement | Enables OpenShift worker nodes to safely use swap for virtualization workloads; swap requires pre-provisioned dedicated storage to be available | GA | always | - |
| Troubleshooting Panel | Korrel8r observability signal correlation in the console | GA | always | Cluster Observability Operator |
| In-Flight Operations | OperationRuleSet-based coordination for safe concurrent operations | TP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-in-flight-operations=true</code></details> | - |
| Kubelet Performance | Optimized kubelet settings for virtualization workloads | TP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-kubelet-performance-settings=true</code></details> | - |
| CPU Manager | Dedicated CPU pinning for guaranteed QoS workloads | DP | <details><summary>opt-in</summary><code>kubevirtFeatureGate:CPUManager</code><br><code>platform.kubevirt.io/enable-cpu-manager-perf-tunings=true</code></details> | - |
| Kernel Samepage Merging (KSM) zero pages only | Node-level KSM zero-pages-only deduplication with adaptive scan rate and max_ptes_none=0 when KSM is active. Standalone for memory savings on Windows VM workers; recommend thp-tuning on the same nodes for THP performance. | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-ksm-zero-only=true</code><br><code>hcoUnconfigured:spec.virtualization.ksmConfiguration</code></details> | - |
| KubeVirt Metrics Exporter | Per-node VM storage I/O latency collection via QMP, QGA, and eBPF, and detailed KVM and memory statistics | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-kubevirt-metrics-exporter=true</code></details> | Cluster Observability Operator _(recommended)_ |
| Logging | Integrated logging stack with LokiStack and ClusterLogForwarder | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-logging=true</code><br><code>platform.kubevirt.io/enable-audit-logging=true</code></details> | Loki Operator, Red Hat OpenShift Logging Operator |
| MTV Operator | Migration Toolkit for Virtualization | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-mtv=true</code></details> | - |
| MetalLB Operator | Bare-metal load balancer for services | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-metallb=true</code></details> | - |
| NHC with FAR remediator and SBR detection-mode config | Node health checks with fence-agents remediation and storage-based remediation in detection-only mode | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-node-remediation=true</code></details> | Node Health Check Operator, Fence Agents Remediation Operator, Storage Based Remediation Operator |
| PCI Passthrough | GPU/PCI device passthrough via VFIO | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/openshift=true</code><br><code>platform.kubevirt.io/enable-pci-passthrough=true</code></details> | - |
| SCSI Persistent Reservations | Configures worker-node multipath to persist SCSI persistent-reservation keys for VMs sharing LUN-backed disks | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-scsi-persistent-reservations=true</code></details> | - |
| Transparent Huge Pages (THP) Tuning | Node-level THP madvise/defrag tuning, khugepaged scan rates, and max_ptes_none coordination with KSM. Optional sub-feature: enable-thp-tuning-kernelcore sets kernelcore=2G and movablecore=97% (max(2G, 3% MemTotal) ZONE_MOVABLE split per node); requires reboot. | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-thp-tuning=true</code><br><code>platform.kubevirt.io/enable-thp-tuning-kernelcore=true</code></details> | - |
| VM drain shutdown inhibitor | Attempts to gracefully shutdown KubeVirt VMs before allowing the node to shut down | DP | <details><summary>opt-in</summary><code>platform.kubevirt.io/enable-vm-drain-shutdown-inhibitor=true</code></details> | - |
<!-- END FEATURE STATUS -->

## Opt-In Features

Technology Preview (TP) and Developer Preview (DP) capabilities listed as
**opt-in** in the table above are not applied by default; enable them with an
annotation on the HyperConverged CR. Disabling an opt-in feature stops
autopilot from managing it but does not undo cluster changes — cleanup of
created resources is manual.

### Enable

Set the annotation on `HyperConverged/kubevirt-hyperconverged` (in `openshift-cnv`):

```bash
oc annotate hyperconverged kubevirt-hyperconverged -n openshift-cnv \
  platform.kubevirt.io/enable-<feature>=true
```

Autopilot reconciles and creates or updates the managed resources. Verify with
`oc logs -n openshift-cnv deploy/virt-platform-autopilot`.

### Disable

Remove the opt-in annotation (trailing `-`):

```bash
oc annotate hyperconverged kubevirt-hyperconverged -n openshift-cnv \
  platform.kubevirt.io/enable-<feature>-
```

This stops autopilot from managing that feature. It does **not** delete resources
already created.

Resources the feature already created must be removed manually. For example,
MachineConfig-based opt-ins (THP, KSM, swap) require deleting the autopilot
MachineConfigs and waiting for MCO to roll out the reverted node config:

```bash
oc delete mc <machineconfig-name>
oc get mcp
```

The same applies to other shared cluster objects the feature may have changed
(for example `cluster-monitoring-config` for KSM monitoring).

## Quick Start

### Prerequisites

- OpenShift cluster with OpenShift Virtualization (HCO) installed
- `kubectl` or `oc` CLI access
- Go 1.26+ (for development)

### Installation

1. Build and push the image:
```bash
make docker-build docker-push
```

2. Deploy to cluster:
```bash
make deploy
```

3. **(Optional) Disable the autopilot:**

   The autopilot is GA and **enabled by default**, once deployed it starts managing the
   platform without any additional opt-in. To disable it, set the annotation to `false`:

   ```bash
   kubectl annotate hyperconverged kubevirt-hyperconverged -n openshift-cnv \
     platform.kubevirt.io/autopilot=false
   ```

   > **Note:** The autopilot manages resources by default. It goes idle only while
   > `platform.kubevirt.io/autopilot: "false"` is present on the HCO CR; removing the
   > annotation (or setting it to `"true"`) re-enables management.

4. Verify installation:
```bash
kubectl get deployment -n openshift-cnv
kubectl logs -n openshift-cnv deployment/virt-platform-autopilot
```

## How It Works

The autopilot continuously evaluates your infrastructure and applies production best practices. Configuration happens automatically based on hardware detection, platform capabilities, and operational requirements.

**For example:**
- **NUMA topology awareness** for performance optimization
- **Node auto-remediation** for reliability
- **Intelligent workload placement** for resource efficiency
- **Optimized platform defaults** for production stability
- **Swap optimization** for memory management
- **CPU management** for guaranteed workloads
- **Perses monitoring dashboards**, incident detection, and Korrel8r resource correlation via the OpenShift console (when the Cluster Observability Operator is installed)

The configuration adapts to your environment - if hardware capabilities are detected, appropriate optimizations are applied automatically.

## User Control

While the autopilot provides opinionated defaults, you maintain full control through standard Kubernetes annotations.

### GitOps-Friendly Customization

**JSON Patch Override** - Customize any managed resource:
```yaml
apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  name: 90-worker-swap-online
  annotations:
    platform.kubevirt.io/patch: |
      [
        {"op": "replace", "path": "/spec/config/systemd/units/0/contents", "value": "..."}
      ]
```

**Field Masking** - Exclude specific fields from management:
```yaml
apiVersion: hco.kubevirt.io/v1beta1
kind: HyperConverged
metadata:
  annotations:
    platform.kubevirt.io/ignore-fields: "/spec/liveMigrationConfig/parallelMigrationsPerCluster,/spec/featureGates"
```

**Full Opt-Out** - Stop managing a resource entirely:
```yaml
metadata:
  annotations:
    platform.kubevirt.io/mode: unmanaged
```

All customizations are declarative and version-control friendly - perfect for GitOps workflows.

For detailed control mechanisms, see the [Architecture documentation](docs/ARCHITECTURE.md).

## Architecture

The autopilot uses a **"Patched Baseline"** approach:
1. Renders opinionated defaults from templates
2. Applies user customizations via annotations
3. Detects drift and reconciles to desired state

**Key principles:**
- **Zero API Surface**: No new CRDs, no status fields to monitor
- **Silent operation**: Alerts only when user intervention is required
- **GitOps-native**: All control via standard Kubernetes annotations
- **Convention over Configuration**: Opinionated defaults, customizable when needed

**Three-Tier Management:**
1. **Always-On**: Critical baseline configurations (MachineConfig, Kubelet settings)
2. **Context-Aware**: Activated based on conditions (KubeDescheduler, CPU Manager)
3. **Advanced**: Specialized features (VFIO, USB passthrough, AAQ operator)

**For technical details, see:** [ARCHITECTURE.md](docs/ARCHITECTURE.md)

## Documentation

- [Architecture Deep-Dive](docs/ARCHITECTURE.md) - Technical implementation details, design philosophy, reconciliation flow
- [Adding Assets](docs/adding-assets.md) - Guide for extending the platform with new components
- [Local Development](docs/local-development.md) - Setting up dev environment with Kind
- [Lifecycle Management](docs/lifecycle-management.md) - Tombstoning and resource exclusions
- [Debug Endpoints](docs/debug-endpoints.md) - Debugging and inspection tools
- [SCSI Persistent Reservations](docs/scsi-persistent-reservations.md) - Configure multipath reservation keys for shared LUN disks
- [MachineConfig Rollout Coalescing](docs/machineconfig-rollout-coalescing.md) - Stage expensive MachineConfig updates for an active MCP rollout
- [Runbooks](https://github.com/kubevirt/monitoring/tree/main/docs/runbooks) - Operational guides for alerts (published at https://kubevirt.io/monitoring/runbooks/)

## Contributing

Contributions are welcome! To add new platform components or extend the autopilot, see the [Adding Assets Guide](docs/adding-assets.md).

**Development workflow:**
1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests: `make test`
5. Submit a pull request

**Development commands:**

```bash
# Build locally
make build

# Run tests
make test                  # Unit tests
make test-integration      # Integration tests

# Local development with Kind
make kind-setup            # Setup local cluster
make deploy-local          # Deploy autopilot
make logs-local            # View logs
make redeploy-local        # Redeploy after changes

# Development cycle (format + test + redeploy)
make dev-cycle
```

See [Local Development Guide](docs/local-development.md) for complete instructions, including deploying custom builds to OpenShift clusters.

## License

Apache License 2.0
