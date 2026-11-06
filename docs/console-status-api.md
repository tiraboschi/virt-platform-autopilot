# Console Status API

The Autopilot status API is a supported, read-only HTTP API for the OpenShift Console plugin, support tools, and automation. It is served by the existing manager, not a separate microservice. It does not add a CRD or a status field.

## Security and access

`GET /status/v1beta1` is served only by the `virt-platform-autopilot-status` Service on HTTPS port 8444. The Service requests a service-ca serving certificate; no Route is created. The server watches that certificate for rotation and applies the cluster TLS profile. The existing `/debug/*` listener remains localhost-only.

The ConsolePlugin Service proxy must use `authorization: UserToken`. Console verifies the service certificate and forwards the logged-in user's bearer token. The manager performs a `TokenReview`, followed by a `SubjectAccessReview` for `get` on non-resource URL `/status/v1beta1`. A missing token receives `401`; an invalid or unauthorized token receives `403`. We deliberately do **not** require client mTLS here: it authenticates one Console workload rather than the human Console user. Metrics retain their independent mTLS policy.

The manager service account needs `create` on `tokenreviews` and `subjectaccessreviews`. Administrators grant readers:

```yaml
rules:
  - nonResourceURLs: ["/status/v1beta1"]
    verbs: ["get"]
```

Initially bind that permission only to cluster administrators. Do not include rendered objects, raw patches, or ignored-field values in this API.

## Console configuration

```yaml
spec:
  proxy:
    - alias: autopilot-status
      authorization: UserToken
      endpoint:
        type: Service
        service:
          name: virt-platform-autopilot-status
          namespace: openshift-cnv
          port: 8444
```

The plugin calls `/api/proxy/plugin/kubevirt-plugin/autopilot-status/status/v1beta1` through `consoleFetch`.

## Desired configuration detail

`GET /status/v1beta1/assets/{assetName}` returns the cached effective desired object for one asset. It is populated during reconciliation after validated JSON patches and ignored-field masking, so it is suitable for a Console side-by-side view with the live object. The Console must read the live object through the Kubernetes API under the user's ordinary resource RBAC. Sensitive kinds such as `Secret` are never returned by this endpoint, and `mode: unmanaged` assets have no desired object.

Feature entries include a catalog `id` and `assets` list. Each reconciled asset exposes its `id` and `desiredURL`, allowing Console to discover the relationship without parsing display text or maintaining an Autopilot resource catalog. An asset record supplies its `apiVersion`, `kind`, `namespace`, and `name`; Console resolves the corresponding Kubernetes resource through API discovery, then obtains its live state under the user's ordinary resource RBAC. The `assets` list may include an asset that is absent from the snapshot because it is disabled, excluded, or not yet reconciled.

Annotation-only opt-in features additionally return a complete `activation` action when an HCO is available. Its `target` identifies the exact current HCO object (group, version, resource, kind, namespace, and name), and its merge `patch` enables the feature. Console can render that action without hard-coding the HCO namespace, name, annotation, or patch shape. On an administrator's confirmation, Console applies the supplied patch directly to `target` using the user's normal Kubernetes credentials; it should use an access review or handle a forbidden response when the user lacks `patch` permission. The status API never applies the patch and holds no write authority. Features requiring a KubeVirt feature gate or another non-annotation prerequisite intentionally have no one-click activation action.

## v1beta1 compatibility contract

`v1beta1` is the supported beta contract. It may gain optional fields and new enum values, but existing fields will not be removed, renamed, or assigned a different meaning. A breaking change uses a new path and `apiVersion`; it does not silently alter `v1beta1`. Clients must ignore unknown fields and treat an unknown enum value as `unknown`, preserving the raw value for diagnostics. `observedAt` is snapshot freshness, not request time; a response with `ready: false` or `stale: true` must not be presented as current cluster state.

`assets[].state` and `features[].availability` are extensible enums. The following values are defined in `v1beta1`:

| Asset state | Console meaning | Desired object | Console action |
| --- | --- | --- | --- |
| `synced` | Effective desired state is applied | available | Compare with live object |
| `drifted` | Drift was detected; reconciliation is in progress | available | Refresh and show remediation progress |
| `applyFailed` | Reconciliation could not apply or verify the asset | available | Surface `reason` and diagnostics |
| `staged` | A safe MachineConfig rollout is pending | available | Show rollout wait state |
| `excluded` | The asset is not applicable | unavailable | Surface `reason`; do not offer configuration |
| `unmanaged` | The administrator opted this object out | unavailable | Explain that Autopilot will not modify it |

Feature availability is derived from the feature's `evidence`, not from UI-maintained resource knowledge. `disabled` means its opt-in configuration is absent; `blocked` means no feature asset is applicable; `pending` means at least one applicable asset is unobserved or staged; `degraded` means an asset is drifted or failed; `healthy` means at least one applicable asset is synced and none is pending, drifted, or failed; `unmanaged` means its applicable assets are user-managed; and `unknown` means the controller has insufficient information. Console should show the evidence list for explanatory detail. An `activation` action is the only signal that Console may offer one-click enablement; its absence is not an error and must not be inferred.

## Response

```json
{
  "apiVersion": "autopilot.kubevirt.io/status/v1beta1",
  "observedAt": "2026-11-06T10:00:00Z",
  "ready": true,
  "lastSuccessfulReconcile": "2026-11-06T10:00:00Z",
  "stale": false,
  "features": [{"id":"logging","name":"Logging","maturity":"DP","install":"opt-in","enabled":false,"availability":"disabled","reason":"notConfigured","assets":["cluster-log-forwarder"],"activation":{"target":{"group":"hco.kubevirt.io","version":"v1","resource":"hyperconvergeds","kind":"HyperConverged","namespace":"openshift-cnv","name":"kubevirt-hyperconverged"},"method":"PATCH","patchType":"application/merge-patch+json","patch":{"metadata":{"annotations":{"platform.kubevirt.io/enable-logging":"true"}}}}}],
  "assets": [{"id":"swap-enable","apiVersion":"machineconfiguration.openshift.io/v1","kind":"MachineConfig","name":"90-worker-swap-online","namespace":"","state":"synced","desiredURL":"/status/v1beta1/assets/swap-enable","lastTransitionTime":"2026-11-06T10:00:00Z","lastDriftTime":"2026-11-06T09:59:00Z"}]
}
```

The elected reconciler rebuilds the in-memory snapshot during reconciliation. Until its first successful reconciliation, `ready` is false and the Console should display an initializing state. `observedAt` is the time of that successful reconciliation—not the HTTP response time. A later failure sets `lastFailedReconcile` and `stale: true`; the UI must present the prior snapshot as stale. The deployment intentionally has one replica; leader-only Service routing is required before this endpoint can be made highly available. `enabled` is configured intent, while `availability` is derived from the current asset evidence. `lastDriftTime` is retained after an asset returns to `synced`, so the Console can report recent remediation without triggering SSA dry-run.

## Integration examples

### Healthy, blocked, and degraded features

A healthy feature has at least one applicable synced asset and no pending, drifted, or failed asset. Excluded assets can be topology-specific and therefore do not make an otherwise healthy feature unhealthy:

```json
{
  "id": "scsi-persistent-reservations",
  "enabled": true,
  "availability": "healthy",
  "assets": ["scsi-persistent-reservations", "scsi-persistent-reservations-master"],
  "evidence": [
    {"assetID": "scsi-persistent-reservations", "state": "synced"},
    {"assetID": "scsi-persistent-reservations-master", "state": "excluded", "reason": "conditionNotMet"}
  ]
}
```

A blocked feature has no applicable asset. The Console should surface the evidence and avoid offering a desired-object comparison:

```json
{
  "id": "logging",
  "enabled": true,
  "availability": "blocked",
  "reason": "noApplicableAssets",
  "evidence": [
    {"assetID": "logging-lokistack", "state": "excluded", "reason": "missingCRD"}
  ]
}
```

A degraded feature identifies the currently failing asset. `desiredURL` from that asset record remains suitable for a side-by-side inspection:

```json
{
  "id": "swap-enablement",
  "enabled": true,
  "availability": "degraded",
  "reason": "applyFailed",
  "evidence": [
    {"assetID": "swap-enable", "state": "applyFailed", "reason": "applyFailed"}
  ]
}
```

### Desired versus live object

For an asset with `desiredURL`, Console fetches the effective desired object from the status proxy and reads the live object from the Kubernetes API using the asset's `apiVersion`, `kind`, `namespace`, and `name`. The latter request is intentionally made with the user's ordinary Kubernetes resource permissions.

```ts
const snapshot = await consoleFetch(statusURL).then((r) => r.json());
const asset = snapshot.assets.find((item) => item.id === 'swap-enable');

const desired = await consoleFetch(asset.desiredURL).then((r) => r.json());
const live = await k8sGet({
  apiVersion: asset.apiVersion,
  kind: asset.kind,
  namespace: asset.namespace || undefined,
  name: asset.name,
});
renderDiff(live, desired);
```

`desiredURL` is omitted for excluded and unmanaged assets. A `404` from the desired endpoint has the same meaning: no currently cached effective desired object exists; it is not a request to render one.

### One-click opt-in

Console offers an Enable action only when `feature.activation` is present. It displays the target and patch for confirmation, verifies (or attempts and handles) the user's `patch` permission, then applies the supplied merge patch unchanged:

```ts
if (feature.activation) {
  const { target, method, patchType, patch } = feature.activation;
  await k8sPatch({
    apiVersion: `${target.group}/${target.version}`,
    kind: target.kind,
    namespace: target.namespace,
    name: target.name,
  }, patch, patchType);
  // Re-fetch the snapshot; reconciliation updates feature state asynchronously.
}
```

No `activation` action is returned for features that need a KubeVirt feature gate or another prerequisite. Console must show the requirement, not synthesize a patch.

### HTTP and retry behavior

| Condition | Status | Console behavior |
| --- | --- | --- |
| Authorized snapshot | `200` | Render it, respecting `ready` and `stale`. |
| No bearer token | `401` | Treat as authentication/proxy configuration failure. |
| Token invalid or caller lacks status permission | `403` | Show no Autopilot data; do not fall back to privileged access. |
| Unknown or unavailable desired asset | `404` | Hide comparison; do not invoke debug rendering. |
| Unsupported method | `405` | Client bug; do not retry. |
| Status Service unavailable | connection failure or proxy error | Retry with bounded exponential backoff; retain a previously displayed snapshot only when marked stale. |

The API does not currently return an HTTP `503` for a non-leader: the deployment is intentionally single replica. A future HA routing design may add that behavior in a new documented contract revision.

### RBAC examples

A viewer needs only the non-resource status permission; live-object reads remain governed by the cluster's usual per-resource RBAC.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: autopilot-status-viewer
rules:
  - nonResourceURLs: ["/status/v1beta1"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: autopilot-status-viewer-example
subjects:
  - kind: Group
    name: example:autopilot-viewers
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: autopilot-status-viewer
```

One-click activation additionally requires normal `patch` access to the namespaced HCO object. This should be granted independently and only to the intended administrator group:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: autopilot-hco-activator
  namespace: openshift-cnv
rules:
  - apiGroups: ["hco.kubevirt.io"]
    resources: ["hyperconvergeds"]
    resourceNames: ["kubevirt-hyperconverged"]
    verbs: ["get", "patch"]
```

## Operational constraints

The endpoint is a snapshot API: requests never render assets, apply objects, or run SSA dry-run. This prevents Console polling from creating API load or changing reconciliation behavior. The API is unavailable until the service-ca certificate is mounted and the manager restarts.
