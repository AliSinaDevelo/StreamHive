# Deployment

## Container

Build and run (example flags):

```bash
docker build -t streamhive:local .
docker run --rm -p 7070:7070 -p 8080:8080 streamhive:local \
  -listen 0.0.0.0:7070 \
  -health 0.0.0.0:8080
```

- **7070** — P2P TCP listener (example).
- **8080** — HTTP `/livez`, `/readyz`, `/metrics` (JSON counters).

Use TLS flags (`-tls-cert`, `-tls-key`, `-tls-ca`, …) when exposing services beyond a lab network.

## Kubernetes (minimal)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: streamhive
spec:
  replicas: 1
  selector:
    matchLabels:
      app: streamhive
  template:
    metadata:
      labels:
        app: streamhive
    spec:
      containers:
        - name: streamhive
          image: streamhive:local
          args: ["-listen", "0.0.0.0:7070", "-health", "0.0.0.0:8080"]
          ports:
            - containerPort: 7070
              name: p2p
            - containerPort: 8080
              name: health
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /livez
              port: health
            initialDelaySeconds: 2
            periodSeconds: 10
```

Add a `Service` for the health port and (separately) headless or load-balanced service for P2P depending on your topology. Tune resource requests/limits and pod anti-affinity for HA; this manifest is illustrative only.

## SLOs

Define error budgets once you expose a workload to users. Baseline probes:

- **Availability**: `/livez` success rate.
- **Readiness**: `/readyz` reflects listener bound (`TCPTransport.Ready`).
- **Saturation**: JSON `/metrics` field `active_peers` and `peers_rejected`.
