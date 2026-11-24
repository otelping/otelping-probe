# otelping-probe

Lightweight, zero-dependency internal synthetic monitoring probe written in pure Go. Runs inside private Kubernetes clusters, VPC subnets, Docker hosts, or office LANs and pushes HTTP/HTTPS, TLS certificate expiry, TCP port, and split-horizon DNS health metrics outbound via standard **OpenTelemetry (`OTLP/HTTP`)**.

Built for **[OTelPing](https://otelping.eu)** (10-second European multi-cloud uptime and OpenTelemetry monitoring, 100% hosted in the EU) and compatible with any standard OpenTelemetry Collector (`/v1/metrics`).

## Why `otelping-probe`?

- **Zero inbound firewall ports**: Connects strictly outbound over HTTPS (`443`) to `https://app.otelping.eu/v1/metrics` (or your own OTLP collector).
- **Zero third-party Go dependencies**: Pure Go standard library (`CGO_ENABLED=0`), auditable in minutes, ~6 MB static binary, distroless non-root container (`< 25 MiB` RAM).
- **Private PKI & Service Groups**: Supports custom internal root CAs (`tls_ca_file` for HashiCorp Vault / Active Directory / step-ca), W3C `traceparent` headers, sub-millisecond DNS/TCP/TLS/TTFB timings, TLS expiry day tracking, and OTelPing Service Groups.

## Quickstart (Zero Config File)

Grab your workspace API key (`otp_live_...`) from **[app.otelping.eu](https://app.otelping.eu)** and run:

```bash
export OTELPING_API_KEY="otp_live_your_workspace_key"
export OTELPING_REGION="k8s-eu-internal"
export OTELPING_TARGETS="Payments-API=https://payments.internal:8443/healthz,Postgres-Primary=tcp://10.20.1.15:5432"

go run github.com/otelping/otelping-probe/cmd/otelping-probe@latest
```

Run once for CI/CD pipelines or Kubernetes CronJobs:

```bash
otelping-probe --config otelping.yaml --once
```

## Configuration (`otelping.yaml`)

Copy [`otelping.example.yaml`](./otelping.example.yaml) to `otelping.yaml`:

```yaml
endpoint: "https://app.otelping.eu"
api_key: "${OTELPING_API_KEY}"
region: "eu-corp-vlan-01"
interval: "10s"

checks:
  - name: "Internal Payments API"
    group: "Core-Backend"
    kind: "http"
    target: "https://payments.internal.company.eu/healthz"
    keyword: "ok"
    timeout: "5s"
    tls_ca_file: "/etc/otelping/internal-ca.pem"

  - name: "Primary PostgreSQL"
    group: "Core-Backend"
    kind: "tcp"
    target: "tcp://10.20.1.15:5432"

  - name: "Split-Horizon Internal DNS"
    kind: "dns"
    target: "dns://payments.internal.company.eu"
    dns_server: "10.20.0.53:53"
    interval: "30s"
```

## Deployment

- **Docker Compose**: [`docker-compose.yml`](./docker-compose.yml) (`docker compose up -d`)
- **Kubernetes**: [`deploy/k8s/deployment.yaml`](./deploy/k8s/deployment.yaml)
- **Linux systemd**: [`deploy/systemd/otelping-probe.service`](./deploy/systemd/otelping-probe.service)

## Exported OpenTelemetry Metrics (`OTLP/HTTP`)

Each check emits a `ResourceMetrics` entry (`service.name`, `service.group`, `probe.region`, `check.kind`, `url.full`, `trace.id`) containing:

| Metric | Type | Unit | Description |
| :--- | :--- | :--- | :--- |
| `check.passed` | Gauge (Int) | `1` | `1` when healthy, `0` when failed |
| `http.client.duration` | Gauge (Double) | `ms` | End-to-end check duration |
| `http.response.status_code` | Gauge (Int) | `1` | HTTP status code (or `200`/`503` for TCP/DNS) |
| `net.dns.duration` | Gauge (Double) | `ms` | DNS lookup latency |
| `net.tls.duration` | Gauge (Double) | `ms` | TLS handshake latency |
| `http.ttfb.duration` | Gauge (Double) | `ms` | Time to first response byte |
| `tls.cert.days_remaining` | Gauge (Int) | `d` | Days until leaf X.509 certificate expires |

## Related European Platforms

- **[OTelPing (`otelping.eu`)](https://otelping.eu)**: 10-second European web, load-balancer, DNS, and cron uptime monitoring with native OpenTelemetry (`EUR 39/mo`).
- **[InferPing (`inferping.eu`)](https://inferping.eu)** & **[`inferping-probe`](https://github.com/inferping/inferping-probe)**: 10-second European AI gateway, LLM edge latency, and `#mcp-gate` Model Context Protocol security monitoring (`EUR 39/mo`).
- **[Vallocor (`vallocor.eu`)](https://vallocor.eu)**: Independent European cybersecurity watchpost (`Outpost` / `Sentinel` / `Bastion`) and on-site `Vallocor Pulse` sensor alongside SMEs and their IT partners.

## License

Apache License 2.0 - Built in the Netherlands (EU) by **[OTelPing](https://otelping.eu)** (Vallocor Security Engineering / GREYPIXEL ICT).
