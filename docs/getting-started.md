# Getting Started with Sartor

This guide will help you get Sartor up and running in your Kubernetes cluster.

## Prerequisites

Before installing Sartor, ensure you have:

- **Kubernetes Cluster** (v1.11.3 or later)
- **Prometheus** with container metrics exposed
- **Git Repository** access (GitHub, GitLab, or Bitbucket)
- **kubectl** configured to access your cluster

## Installation

### Option 1: Helm (Recommended)

```bash
# Add the Helm repository
helm repo add sartor https://sartorproj.github.io/sartor
helm repo update

# Install Sartor
helm install sartor sartor/sartor \
  --namespace sartor-system \
  --create-namespace \
  --set prometheus.url=http://prometheus:9090 \
  --set gitProvider.type=github \
  --set gitProvider.secretRef.name=github-token
```

### Option 2: kubectl

```bash
# Install CRDs and controller
kubectl apply -f https://github.com/sartorproj/sartor/releases/latest/download/install.yaml

# Or from source
make install
make deploy IMG=sartor-controller:latest
```

## Configuration

### Step 1: Create Git Provider Secret

```bash
kubectl create secret generic github-token \
  --from-literal=token=YOUR_GITHUB_TOKEN \
  -n sartor-system
```

### Step 2: Create Atelier

```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: Atelier
metadata:
  name: default
spec:
  prometheus:
    url: http://prometheus.monitoring.svc.cluster.local:9090
  gitProvider:
    type: github
    secretRef:
      name: github-token
      namespace: sartor-system
  prSettings:
    cooldownPeriod: 168h  # 7 days
    minChangePercent: 20
  safetyRails:
    minCPU: "10m"
    minMemory: "64Mi"
```

Apply it:

```bash
kubectl apply -f atelier.yaml
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
  intent: Balanced
  writeBack:
    strategy: RawYAML
    repository: "myorg/manifests"
    path: "apps/my-app/deployment.yaml"
    branch: "main"
  analysisWindow: 168h  # 7 days
```

Apply it:

```bash
kubectl apply -f tailoring.yaml
```

## Verification

Check that Sartor is working:

```bash
# Check controller is running
kubectl get pods -n sartor-system

# Check Atelier status
kubectl get atelier default -o yaml

# Check Tailoring status
kubectl get tailoring my-app -n production -o yaml

# Watch for Cut resources (PRs)
kubectl get cuts -n production -w
```

## Next Steps

- Learn about [Intent Profiles](intent-profiles.md)
- Configure [GitOps Integration](gitops-integration.md)
- Set up [ArgoCD](argocd-setup.md)
- Explore the [API Reference](api-reference.md)

## Troubleshooting

See the [Troubleshooting Guide](troubleshooting.md) for common issues.
