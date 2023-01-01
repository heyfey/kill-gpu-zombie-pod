# kill-gpu-zombie-pod

Detect and delete GPU zombie pod in Kubernetes cluster.

In a GPU cluster, if a GPU is scheduled to a pod but has zero utilization continuously, the pod may be hanging or deadlocked, which harms the cluster utilization. 

This repository provides a tool to automatically detect and delete these "GPU zombie pods" after a timeout period.

## Quick Start

### 1. Deploy `nvidia_smi_exporter`

We scratch GPU metrics from [heyfey/nvidia_smi_exporter](https://github.com/heyfey/nvidia_smi_exporter)

```
git clone https://github.com/heyfey/nvidia_smi_exporter.git
kubectl apply -f nvidia_smi_exporter/nvidia_smi_exporter.yaml 
```

### 2. Deploy `kill-gpu-zombie-pod`
```
git clone https://github.com/heyfey/kill-gpu-zombie-pod.git
cd kill-gpu-zombie-pod
kubectl apply -f kill-gpu-zombie-pod.yaml
```

args:
```
-check_period_seconds float
        Check for zombie every # seconds (default 10)
-idle_timeout_seconds float
        Kill the pod after idle timeout of # seconds (default 90)
-namespace string
        Detect and kill GPU zombie pod in the namespace (default "default")
```

You can specify args in the [YAML](https://github.com/heyfey/kill-gpu-zombie-pod/blob/main/kill-gpu-zombie-pod.yaml)

### 3. Done!

## Zombie Example

```
kubectl apply -f gpu-zombie-pod.yaml
```

## Build Image

```
docker build  .
```