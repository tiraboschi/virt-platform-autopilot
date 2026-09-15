apiVersion: perses.dev/v1alpha2
kind: PersesDashboard
metadata:
  labels:
    platform.kubevirt.io/managed-by: virt-platform-autopilot
  name: autopilot-asset-health
  namespace: {{ .HCO.GetNamespace | default "openshift-cnv" }}
spec:
  config:
    display:
      name: Autopilot / Asset Health
    duration: 6h
    layouts:
    - kind: Grid
      spec:
        display:
          collapse:
            open: true
          title: Active Alerts
        items:
        - content:
            $ref: '#/spec/panels/5_0'
          height: 8
          width: 24
          x: 0
          "y": 0
    - kind: Grid
      spec:
        display:
          collapse:
            open: true
          title: Compliance Overview
        items:
        - content:
            $ref: '#/spec/panels/0_0'
          height: 10
          width: 12
          x: 0
          "y": 0
        - content:
            $ref: '#/spec/panels/0_1'
          height: 10
          width: 12
          x: 12
          "y": 0
    - kind: Grid
      spec:
        display:
          collapse:
            open: true
          title: Edit Wars & Paused Assets
        items:
        - content:
            $ref: '#/spec/panels/1_1'
          height: 9
          width: 12
          x: 0
          "y": 0
        - content:
            $ref: '#/spec/panels/1_0'
          height: 9
          width: 12
          x: 12
          "y": 0
    - kind: Grid
      spec:
        display:
          collapse:
            open: true
          title: Staged MachineConfig Updates
        items:
        - content:
            $ref: '#/spec/panels/6_0'
          height: 9
          width: 24
          x: 0
          "y": 0
    - kind: Grid
      spec:
        display:
          collapse:
            open: true
          title: Customizations & Dependencies
        items:
        - content:
            $ref: '#/spec/panels/3_0'
          height: 9
          width: 12
          x: 0
          "y": 0
        - content:
            $ref: '#/spec/panels/3_1'
          height: 9
          width: 12
          x: 12
          "y": 0
    - kind: Grid
      spec:
        display:
          collapse:
            open: true
          title: Reconciliation Performance
        items:
        - content:
            $ref: '#/spec/panels/2_1'
          height: 9
          width: 12
          x: 0
          "y": 0
        - content:
            $ref: '#/spec/panels/2_2'
          height: 9
          width: 12
          x: 12
          "y": 0
    - kind: Grid
      spec:
        display:
          collapse:
            open: true
          title: Controller Errors
        items:
        - content:
            $ref: '#/spec/panels/4_0'
          height: 9
          width: 24
          x: 0
          "y": 0
    panels:
      "0_0":
        kind: Panel
        spec:
          display:
            description: Current compliance status of each managed asset. Synced (1)
              means the live resource matches the golden state. Drifted (0) means
              a mismatch was detected.
            name: Current Asset Compliance Status
          plugin:
            kind: Table
            spec:
              columnSettings:
              - hide: true
                name: timestamp
              - enableSorting: true
                header: Asset
                name: name
              - enableSorting: true
                header: Kind
                name: kind
              - enableSorting: true
                header: Namespace
                name: namespace
              - cellSettings:
                - condition:
                    kind: Value
                    spec:
                      value: "1"
                  text: Synced
                  textColor: '#73BF69'
                - condition:
                    kind: Value
                    spec:
                      value: "0"
                  text: Drifted
                  textColor: '#F2495C'
                enableSorting: true
                header: Status
                name: value
              - hide: true
                name: __name__
              - hide: true
                name: clusterID
              - hide: true
                name: container
              - hide: true
                name: endpoint
              - hide: true
                name: instance
              - hide: true
                name: job
              - hide: true
                name: pod
              - hide: true
                name: receive
              - hide: true
                name: service
              - hide: true
                name: tenant_id
              - hide: true
                name: cluster_domain
              transforms:
              - kind: MergeSeries
                spec: {}
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: kubevirt_autopilot_compliance_status
      "0_1":
        kind: Panel
        spec:
          display:
            description: "Compliance status over time. 1 = Synced (asset matches golden\
              \ state) | 0 = Drifted (mismatch detected). Drops from 1→0 indicate\
              \ drift events; recovery back to 1 indicates autopilot re-applied the\
              \ golden state."
            name: Compliance Over Time
          plugin:
            kind: TimeSeriesChart
            spec:
              legend:
                mode: table
                position: right
                values:
                - last-number
                - min
              visual:
                areaOpacity: 0.1
                display: line
                lineWidth: 1
              yAxis:
                format:
                  unit: decimal
                max: 1.1
                min: 0
                show: true
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: kubevirt_autopilot_compliance_status
                  seriesNameFormat: '{{"{{name}}"}} ({{"{{kind}}"}})'
      "1_0":
        kind: Panel
        spec:
          display:
            description: Rate of reconciliation throttling events per asset. Spikes
              indicate an active edit war where external changes conflict with the
              autopilot golden state.
            name: Thrashing Rate
          plugin:
            kind: TimeSeriesChart
            spec:
              legend:
                mode: table
                position: right
                values:
                - last-number
                - max
              visual:
                areaOpacity: 0.1
                display: line
                lineWidth: 1
              yAxis:
                format:
                  unit: ops/sec
                show: true
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: rate(kubevirt_autopilot_thrashing_total[5m])
                  seriesNameFormat: '{{"{{name}}"}} ({{"{{kind}}"}})'
      "1_1":
        kind: Panel
        spec:
          display:
            description: Assets currently paused due to edit war detection. Requires
              human intervention to unpause — remove the reconcile-paused annotation
              from the resource. Empty table means no assets are paused.
            name: Currently Paused Assets
          plugin:
            kind: Table
            spec:
              columnSettings:
              - hide: true
                name: timestamp
              - enableSorting: true
                header: Asset
                name: name
              - enableSorting: true
                header: Kind
                name: kind
              - enableSorting: true
                header: Namespace
                name: namespace
              - hide: true
                name: value
              - hide: true
                name: __name__
              - hide: true
                name: clusterID
              - hide: true
                name: container
              - hide: true
                name: endpoint
              - hide: true
                name: instance
              - hide: true
                name: job
              - hide: true
                name: pod
              - hide: true
                name: receive
              - hide: true
                name: service
              - hide: true
                name: tenant_id
              - hide: true
                name: cluster_domain
              transforms:
              - kind: MergeSeries
                spec: {}
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: kubevirt_autopilot_paused_resources == 1
      "2_1":
        kind: Panel
        spec:
          display:
            description: Average time to reconcile each asset (rendering + SSA apply).
              High latency implies API server stress or complex asset rendering.
            name: Average Reconciliation Loop Duration
          plugin:
            kind: TimeSeriesChart
            spec:
              legend:
                mode: table
                position: right
                values:
                - last-number
                - max
              visual:
                areaOpacity: 0.1
                display: line
                lineWidth: 1
              yAxis:
                format:
                  unit: seconds
                show: true
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: rate(kubevirt_autopilot_reconcile_duration_seconds_sum[5m])
                    / rate(kubevirt_autopilot_reconcile_duration_seconds_count[5m])
                  seriesNameFormat: '{{"{{name}}"}} ({{"{{kind}}"}})'
      "2_2":
        kind: Panel
        spec:
          display:
            description: "p95: 95% of reconciliations for that asset finished faster\
              \ than this value — only the slowest 5% took longer. p99: only the slowest\
              \ 1% took longer. Useful to detect outliers invisible in the average:\
              \ a stable average with a high p99 means occasional spikes (API server\
              \ latency, GC pauses, large asset payloads). Both rising together means\
              \ general degradation. Both stable means predictable reconciliation performance."
            name: Reconciliation Latency Percentiles (p95 / p99)
          plugin:
            kind: TimeSeriesChart
            spec:
              legend:
                mode: table
                position: right
                values:
                - last-number
                - max
              visual:
                areaOpacity: 0.1
                display: line
                lineWidth: 1
              yAxis:
                format:
                  unit: seconds
                show: true
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: histogram_quantile(0.95, rate(kubevirt_autopilot_reconcile_duration_seconds_bucket[5m]))
                  seriesNameFormat: 'p95 {{"{{name}}"}} ({{"{{kind}}"}})'
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: histogram_quantile(0.99, rate(kubevirt_autopilot_reconcile_duration_seconds_bucket[5m]))
                  seriesNameFormat: 'p99 {{"{{name}}"}} ({{"{{kind}}"}})'
      "3_0":
        kind: Panel
        spec:
          display:
            description: Assets with intentional customizations (patches, ignored
              fields, or unmanaged). These are expected deviations from the golden
              state.
            name: Current Active Customizations
          plugin:
            kind: Table
            spec:
              columnSettings:
              - hide: true
                name: timestamp
              - enableSorting: true
                header: Asset
                name: name
              - enableSorting: true
                header: Kind
                name: kind
              - enableSorting: true
                header: Namespace
                name: namespace
              - enableSorting: true
                header: Type
                name: type
              - hide: true
                name: value
              - hide: true
                name: __name__
              - hide: true
                name: clusterID
              - hide: true
                name: container
              - hide: true
                name: endpoint
              - hide: true
                name: instance
              - hide: true
                name: job
              - hide: true
                name: pod
              - hide: true
                name: receive
              - hide: true
                name: service
              - hide: true
                name: tenant_id
              - hide: true
                name: cluster_domain
              transforms:
              - kind: MergeSeries
                spec: {}
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: kubevirt_autopilot_customization_info
      "3_1":
        kind: Panel
        spec:
          display:
            description: Optional CRDs that autopilot depends on, and whether the
              feature requiring each one is enabled. Only "Missing, feature enabled"
              is actionable - the associated assets are silently skipped, and it
              raises VirtPlatformAutopilotDependencyMissing. A CRD that is missing
              for a feature nobody enabled is expected and harmless.
            name: Current CRD Dependencies
          plugin:
            kind: Table
            spec:
              columnSettings:
              - hide: true
                name: timestamp
              - enableSorting: true
                header: Kind
                name: kind
              - enableSorting: true
                header: API Group
                name: group
              - enableSorting: true
                header: Version
                name: version
              # Status encodes both facts from the query below:
              #   missing_dependency + 2 * dependency_opted_in
              # 0=present/disabled, 1=missing/disabled, 2=present/enabled,
              # 3=missing/enabled (the only actionable state).
              - cellSettings:
                - condition:
                    kind: Value
                    spec:
                      value: "0"
                  text: Present, feature not enabled
                  textColor: '#8E8E8E'
                - condition:
                    kind: Value
                    spec:
                      value: "1"
                  text: Missing, feature not enabled
                  textColor: '#8E8E8E'
                - condition:
                    kind: Value
                    spec:
                      value: "2"
                  text: Present, feature enabled
                  textColor: '#73BF69'
                - condition:
                    kind: Value
                    spec:
                      value: "3"
                  text: Missing, feature enabled
                  textColor: '#F2495C'
                enableSorting: true
                header: Status
                name: value
              - hide: true
                name: __name__
              - hide: true
                name: clusterID
              - hide: true
                name: container
              - hide: true
                name: endpoint
              - hide: true
                name: instance
              - hide: true
                name: job
              - hide: true
                name: pod
              - hide: true
                name: receive
              - hide: true
                name: service
              - hide: true
                name: tenant_id
              - hide: true
                name: cluster_domain
              transforms:
              - kind: MergeSeries
                spec: {}
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: kubevirt_autopilot_missing_dependency
                    + on(group, version, kind)
                      2 * kubevirt_autopilot_dependency_opted_in
      "4_0":
        kind: Panel
        spec:
          display:
            description: Cumulative count of reconciliation errors in the platform
              controller (controller-runtime). A flat line means zero errors. A rising
              slope means the reconcile loop is failing repeatedly — check operator
              logs for root cause (API server failures, permission denied, SSA conflicts).
            name: Reconcile Errors
          plugin:
            kind: TimeSeriesChart
            spec:
              legend:
                mode: list
                position: bottom
              visual:
                areaOpacity: 0.1
                display: line
                lineWidth: 1
              yAxis:
                format:
                  unit: decimal
                show: true
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: controller_runtime_reconcile_errors_total{controller="platform"}
                  seriesNameFormat: Reconcile Errors
      "5_0":
        kind: Panel
        spec:
          display:
            description: Alerts currently firing for virt-platform-autopilot. Covers
              VirtPlatformAutopilotSyncFailed (critical), VirtPlatformAutopilotThrashingDetected, VirtPlatformAutopilotDependencyMissing,
              and VirtPlatformAutopilotTombstoneStuck (warnings). Empty table means no active
              alerts.
            name: Currently Firing Alerts
          plugin:
            kind: Table
            spec:
              columnSettings:
              - hide: true
                name: timestamp
              - enableSorting: true
                header: Alert
                name: alertname
              - cellSettings:
                - condition:
                    kind: Value
                    spec:
                      value: critical
                  textColor: '#F2495C'
                - condition:
                    kind: Value
                    spec:
                      value: warning
                  textColor: '#FF9830'
                - condition:
                    kind: Value
                    spec:
                      value: info
                  textColor: '#73BF69'
                enableSorting: true
                header: Severity
                name: severity
              - enableSorting: true
                header: Namespace
                name: namespace
              - hide: true
                name: value
              - hide: true
                name: __name__
              - hide: true
                name: alertstate
              - hide: true
                name: clusterID
              - hide: true
                name: cluster_domain
              - hide: true
                name: container
              - hide: true
                name: endpoint
              - hide: true
                name: instance
              - hide: true
                name: job
              - hide: true
                name: pod
              - hide: true
                name: receive
              - hide: true
                name: service
              - hide: true
                name: tenant_id
              transforms:
              - kind: MergeSeries
                spec: {}
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: ALERTS{alertstate="firing",operator="virt-platform-autopilot"}
      "6_0":
        kind: Panel
        spec:
          display:
            description: Existing MachineConfig updates held until a matching MachineConfigPool is already updating. The first matching updating pool releases the change. Empty means no pending update.
            name: Pending MachineConfig Updates
          plugin:
            kind: Table
            spec:
              columnSettings:
              - hide: true
                name: timestamp
              - enableSorting: true
                header: MachineConfig
                name: machineconfig
              - enableSorting: true
                header: Matching MCP
                name: pool
              - hide: true
                name: value
              - hide: true
                name: __name__
              transforms:
              - kind: MergeSeries
                spec: {}
          queries:
          - kind: TimeSeriesQuery
            spec:
              plugin:
                kind: PrometheusTimeSeriesQuery
                spec:
                  query: kubevirt_autopilot_machineconfig_update_staged == 1
