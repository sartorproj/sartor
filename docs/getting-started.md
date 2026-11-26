# Getting Started with Sartor

This guide gets Sartor running in dry-run mode first (recommendations in status only), then enables GitHub write-back.

## Prerequisites

- **Kubernetes cluster**
- **Prometheus-compatible metrics** with cAdvisor container metrics (`container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`). Prometheus and VictoriaMetrics both work.
- **A GitHub repository** with your manifests, Helm values or Kustomize overlays (only for write-back)
- **kubectl** configured to access your cluster

## Installation

### Option 1: Helm

```bash
helm install sartor oci://ghcr.io/sartorproj/charts/sartor \
  --namespace sartor-system \
  --create-namespace
```

The chart source and values live in [sartorproj/sartorproj-helm](https://github.com/sartorproj/sartorproj-helm).

### Option 2: From source

```bash
make install                                    # CRDs
make deploy IMG=ghcr.io/sartorproj/sartor:<tag>  # controller in sartor-system
```

Out of the box the controller runs with `--enable-git-writeback=false` and `--enable-oom-patching=false`: it only reads metrics and writes Tailoring status.

## Configuration

### Step 1: Create an Atelier

```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: Atelier
metadata:
  name: default
spec:
  prometheus:
    url: http://prometheus.monitoring.svc.cluster.local:9090
    # VictoriaMetrics cluster, scoped to one cluster:
    # url: http://vmselect.monitoring.svc:8481/select/0/prometheus?extra_label=cluster=staging-base-cluster
  gitProvider:
    type: github
    secretRef:
      name: github-token
      namespace: sartor-system
  prSettings:
    cooldownPeriod: 168h
    minChangePercent: 20
  safetyRails:
    minCPU: "10m"
    minMemory: "64Mi"
```

```bash
kubectl apply -f atelier.yaml
```

### Step 2: Create a FitProfile

```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: FitProfile
metadata:
  name: percentile-balanced
  namespace: production
spec:
  strategy: percentile
  parameters:
    requestsBufferPercent: 20
    limitsBufferPercent: 30
    maxChangePercent: 50
```

### Step 3: Create Your First Tailoring

```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: Tailoring
metadata:
  name: my-app
  namespace: production
spec:
  target:
    kind: Deployment
    name: my-app
  fitProfileRef:
    name: percentile-balanced
  analysisWindow: 168h
  writeBack:
    repository: github.com/myorg/manifests
    path: apps/my-app/deployment.yaml
    type: raw
    branch: main
```

```bash
kubectl apply -f tailoring.yaml
```

## Verification

```bash
# Controller is running
kubectl get pods -n sartor-system

# Prometheus connectivity
kubectl get atelier default

# Recommendations for the workload
kubectl get tailoring my-app -n production -o jsonpath='{.status.lastAnalysis}'
```

## Enabling Git write-back

1. Create the GitHub credentials Secret in the controller's namespace, either a token or a GitHub App:

   ```bash
   kubectl -n sartor-system create secret generic github-token \
     --from-literal=appId=123456 \
     --from-literal=installationId=7890123 \
     --from-file=privateKey=sartor.private-key.pem
   ```

2. Start the controller with `--enable-git-writeback` (and make sure `POD_NAMESPACE` is set, which the provided manifests do).

3. Watch the Tailoring: `status.prUrl` points to the pull request, and the `PRCreated` condition explains why none was opened (for example `BelowThreshold` or `CooldownActive`).

Set `spec.paused: true` on a Tailoring to keep analyzing it without any Git activity.

## Next Steps

- [Configuration Reference](configuration.md)
- [Recommendation Strategies](recommendation-strategies.md)
- [DSP Strategy](dsp-strategy.md)
