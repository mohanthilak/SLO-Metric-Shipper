# SLO Metric Generator

A lightweight Go application that generates realistic HTTP traffic metrics for testing Service Level Objective (SLO) dashboards on observability platforms. Configure failure rates dynamically through a web UI and export metrics via OpenTelemetry.

## Features

- **5 Simulated API Endpoints**: `/api/users`, `/api/products`, `/api/orders`, `/api/checkout`, `/api/health`
- **Dynamic Configuration**: Adjust status code distributions in real-time via web UI
- **Built-in Load Generation**: Generate traffic directly from the web UI with configurable request rates
- **Real-time Statistics**: Live monitoring of request counts, success rates, and error distributions
- **Multiple Status Codes**: Generates realistic 2xx (success), 4xx (client errors), and 5xx (server errors) responses
- **OpenTelemetry Export**: Metrics exported via OTLP protocol
- **Integrated OTel Collector**: Bundled OpenTelemetry Collector with Docker deployment
- **OTel Config Editor**: Edit collector configuration through the web UI with syntax highlighting
- **Docker Compose Ready**: Complete observability stack with one command
- **No Restart Required**: Configuration changes apply immediately
- **Thread-Safe**: Concurrent request handling with safe configuration updates

## Quick Start (Docker)

The fastest way to get started is with Docker Compose, which includes the application and OpenTelemetry Collector:

```bash
# Start the complete stack
docker-compose up --build

# Access the admin UI
open http://localhost:8080/admin

# View Prometheus metrics from the collector
open http://localhost:8889/metrics
```

That's it! The stack includes:
- SLO Metric Generator on port 8080
- OpenTelemetry Collector with OTLP receiver
- Prometheus Server on port 9090 with persistent storage
- Prometheus metrics exporter on port 8889
- UI-based collector configuration editor

See the [Docker Deployment](#docker-deployment) section for more details.

## Prerequisites

### Docker Deployment (Recommended)
- Docker
- Docker Compose

### Standalone Deployment
- Go 1.21 or higher
- OpenTelemetry Collector (optional, for viewing metrics)

## Installation

### Docker (Recommended)

```bash
# Clone the repository
git clone <repository-url>
cd slo-metric-generator

# Start with Docker Compose
docker-compose up --build
```

### Standalone

1. Clone or download this repository
2. Install dependencies:

```bash
go mod download
```

## Configuration

The application uses environment variables for OpenTelemetry configuration:

- `OTEL_EXPORTER_OTLP_ENDPOINT`: OTLP endpoint (default: `localhost:4317`)
- `OTEL_EXPORTER_OTLP_HEADERS`: Authentication headers if needed (optional)

## Usage

### Starting the Application

```bash
# Basic usage (uses default OTLP endpoint localhost:4317)
go run .

# With custom OTLP endpoint
export OTEL_EXPORTER_OTLP_ENDPOINT="otel-collector.example.com:4317"
go run .

# With authentication
export OTEL_EXPORTER_OTLP_ENDPOINT="otel-collector.example.com:4317"
export OTEL_EXPORTER_OTLP_HEADERS="x-api-key=your-api-key"
go run .
```

The application will start on port 8080 and display available endpoints:

```
Starting SLO Metric Generator on http://localhost:8080
Admin UI available at http://localhost:8080/admin
API endpoints:
  - GET http://localhost:8080/api/users
  - GET http://localhost:8080/api/products
  - GET http://localhost:8080/api/orders
  - GET http://localhost:8080/api/checkout
  - GET http://localhost:8080/api/health
```

### Admin UI

Access the admin interface at `http://localhost:8080/admin` to configure failure rates, generate load, and manage the OpenTelemetry Collector.

#### Status Code Distribution
- **Three sliders per endpoint**:
  - Success Rate (2xx) - green
  - Client Error Rate (4xx) - yellow
  - Server Error Rate (5xx) - red
- **Real-time validation**: Ensures rates sum to 100%
- **Auto-adjustment**: When one slider changes, others adjust automatically
- **Immediate effect**: Changes apply without restarting the application

#### Load Generation
- **Built-in traffic generator**: No need for external tools like curl or hey
- **Configurable request rate**: Set requests per second (1-1000)
- **Total request limit**: Set a specific number of requests or run continuously (0 = unlimited)
- **Real-time statistics**: Live updates showing:
  - Total requests sent
  - Success count (2xx)
  - Client error count (4xx)
  - Server error count (5xx)
  - Elapsed time
  - Current success rate
- **Per-endpoint control**: Start/stop load generation independently for each endpoint
- **Visual indicators**: Loading animations and colored statistics for easy monitoring

#### OpenTelemetry Collector Configuration Editor (Docker only)

When running with Docker Compose, you can edit the OTel Collector configuration directly through the admin UI:

- **Monaco Editor**: Professional code editor with YAML syntax highlighting
- **Real-time Validation**: Checks YAML syntax before saving
- **Automatic Restart**: Collector restarts automatically when configuration is saved
- **Live Preview**: See collector endpoints and health status
- **Safe Editing**: Backup created before each change

To use the editor:
1. Scroll to the "OpenTelemetry Collector Configuration" section in the admin UI
2. Edit the YAML configuration (add exporters, modify pipelines, etc.)
3. Click "Save & Restart Collector" to apply changes
4. The collector will restart with the new configuration (2-3 second downtime)

Example: Add a Prometheus Remote Write exporter to send metrics to Grafana Cloud:

```yaml
exporters:
  prometheusremotewrite:
    endpoint: https://prometheus-us-central1.grafana.net/api/prom/push
    headers:
      authorization: Basic YOUR_BASE64_CREDENTIALS

service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [logging, prometheus, prometheusremotewrite]  # Added new exporter
```

### Generating Traffic

#### Option 1: Using the Built-in Load Generator (Recommended)

The easiest way to generate traffic is through the Admin UI at `http://localhost:8080/admin`:

1. Configure the status code distribution for your endpoint
2. Set the **Requests/Second** (e.g., 10 for moderate load, 100 for high load)
3. Set **Total Requests** (0 for unlimited, or a specific number like 1000)
4. Click **Start** to begin generating traffic
5. Watch real-time statistics update every second
6. Click **Stop** when finished

This method provides:
- Real-time visual feedback
- Automatic statistics tracking
- No need to install external tools
- Easy control and monitoring

#### Option 2: Using curl or Scripts

For automated testing or CI/CD pipelines:

```bash
# Single request
curl http://localhost:8080/api/users

# Generate 100 requests to users endpoint
for i in {1..100}; do
  curl -s http://localhost:8080/api/users > /dev/null
done

# Generate traffic to multiple endpoints
endpoints=("users" "products" "orders" "checkout" "health")
for endpoint in "${endpoints[@]}"; do
  for i in {1..50}; do
    curl -s "http://localhost:8080/api/$endpoint" > /dev/null
  done
done

# With status code output
for i in {1..20}; do
  curl -w "Status: %{http_code}\n" http://localhost:8080/api/users
done
```

#### Option 3: External Load Testing Tools

Use tools like `hey`, `ab`, or `wrk` for sustained load:

```bash
# Using hey (install: go install github.com/rakyll/hey@latest)
hey -n 1000 -c 10 http://localhost:8080/api/users

# Using ab (Apache Bench)
ab -n 1000 -c 10 http://localhost:8080/api/products
```

## Metrics

The application exports the following metric via OpenTelemetry:

### `http_requests_total`

Counter tracking total HTTP requests with labels:
- `endpoint`: API endpoint name (e.g., "users", "products", "orders", "checkout", "health")
- `status_code`: HTTP status code (200, 400, 404, 429, 500, 503)
- `method`: HTTP method (GET)

Example metric output:
```
http_requests_total{endpoint="users",status_code="200",method="GET"} 850
http_requests_total{endpoint="users",status_code="400",method="GET"} 75
http_requests_total{endpoint="users",status_code="500",method="GET"} 75
http_requests_total{endpoint="products",status_code="200",method="GET"} 950
http_requests_total{endpoint="products",status_code="503",method="GET"} 50
```

## OpenTelemetry Collector Setup

Example OpenTelemetry Collector configuration:

```yaml
# otel-collector-config.yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  batch:
    timeout: 10s

exporters:
  # Export to console for testing
  logging:
    loglevel: debug

  # Export to Prometheus
  prometheus:
    endpoint: "0.0.0.0:8889"

  # Export to your observability platform
  # otlp:
  #   endpoint: "your-platform.example.com:4317"

service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [logging, prometheus]
```

Run the collector:

```bash
otelcol --config otel-collector-config.yaml
```

## SLO Calculations

Use the exported metrics to calculate SLOs:

### Availability SLO

```promql
# Success rate (availability)
sum(rate(http_requests_total{status_code=~"2.."}[5m])) /
sum(rate(http_requests_total[5m])) * 100

# Per-endpoint availability
sum(rate(http_requests_total{endpoint="users",status_code=~"2.."}[5m])) /
sum(rate(http_requests_total{endpoint="users"}[5m])) * 100
```

### Error Budget

```promql
# Remaining error budget (assuming 99.9% SLO)
(1 - (
  sum(rate(http_requests_total{status_code=~"2.."}[30d])) /
  sum(rate(http_requests_total[30d]))
)) / (1 - 0.999) * 100
```

### Success vs Error Rates

```promql
# Success rate
sum(rate(http_requests_total{status_code=~"2.."}[5m]))

# Client error rate (4xx)
sum(rate(http_requests_total{status_code=~"4.."}[5m]))

# Server error rate (5xx)
sum(rate(http_requests_total{status_code=~"5.."}[5m]))
```

## Example Scenarios

### Testing High Availability (99.9% uptime)

1. In the Admin UI, set all endpoints to 99.9% success, 0.1% server error
2. Start load generation for all endpoints at 10 req/sec with 0 total requests (unlimited)
3. Watch real-time statistics to verify ~99.9% success rate
4. Check your observability dashboard to confirm ~99.9% availability

### Testing Degraded Service

1. Set `checkout` endpoint to 70% success, 20% 4xx, 10% 5xx
2. Keep other endpoints at 99%+ success
3. Start load generation for all endpoints
4. Monitor how one degraded endpoint affects overall SLO in your dashboard
5. Observe the real-time statistics showing different success rates per endpoint

### Testing Error Budget Burn Rate

1. Configure 95% success rate across all endpoints (5% split between 4xx and 5xx)
2. Start high-volume traffic generation (100+ req/sec per endpoint)
3. Watch the statistics to see rapid error accumulation
4. Calculate error budget consumption over time in your observability platform

### Testing Different Error Types

1. Set `users`: 90% success, 10% 4xx, 0% 5xx (client errors - bad requests, rate limits)
2. Set `orders`: 90% success, 0% 4xx, 10% 5xx (server errors - internal errors, unavailable)
3. Start load generation for both endpoints at the same rate
4. Compare impact of client vs server errors on SLOs in your metrics
5. Use the real-time statistics to verify the error distribution matches your configuration

## Status Code Distribution

The application generates the following status codes:

- **2xx Success**: 200 OK
- **4xx Client Errors**: 400 Bad Request, 404 Not Found, 429 Too Many Requests
- **5xx Server Errors**: 500 Internal Server Error, 503 Service Unavailable

Each error type returns an appropriate JSON response for realistic simulation.

## Architecture

### Standalone Mode
```
┌─────────────────┐
│   Admin UI      │ ← Configure failure rates & load generation
│  (Browser)      │
└────────┬────────┘
         │
    ┌────▼─────────────────────────────┐
    │   HTTP Server (:8080)            │
    │  ┌──────────┐  ┌──────────────┐ │
    │  │   API    │  │    Admin     │ │
    │  │ Handlers │  │   Handlers   │ │
    │  └────┬─────┘  └──────┬───────┘ │
    │       │                │         │
    │  ┌────▼────────────────▼──────┐ │
    │  │   Config Store (sync.RW)   │ │
    │  └────────────┬────────────────┘ │
    │               │                  │
    │  ┌────────────▼────────────────┐ │
    │  │   Metrics (OpenTelemetry)  │ │
    │  └────────────┬────────────────┘ │
    └───────────────┼──────────────────┘
                    │
         ┌──────────▼──────────┐
         │  OTLP Exporter      │
         │  (gRPC :4317)       │
         └──────────┬──────────┘
                    │
         ┌──────────▼──────────┐
         │  External OTel      │
         │  Collector          │
         └─────────────────────┘
```

### Docker Compose Mode (Integrated Stack)
```
┌──────────────────────────────────────────────────────────────┐
│  Docker Compose Network                                      │
│                                                              │
│  ┌─────────────────┐                                         │
│  │   Admin UI      │ ← Configure failure rates, load gen,   │
│  │  (Browser)      │   AND OTel collector config            │
│  └────────┬────────┘                                         │
│           │                                                  │
│      ┌────▼─────────────────────────────────────┐            │
│      │   SLO Generator Container (:8080)        │            │
│      │  ┌──────────┐  ┌───────────────────────┐ │            │
│      │  │   API    │  │    Admin + OTel      │ │            │
│      │  │ Handlers │  │    Config Handler    │ │            │
│      │  └────┬─────┘  └──────┬────────────────┘ │            │
│      │       │                │                  │            │
│      │  ┌────▼────────────────▼──────┐          │            │
│      │  │   Config Store (sync.RW)   │          │            │
│      │  └────────────┬────────────────┘          │            │
│      │               │                           │            │
│      │  ┌────────────▼────────────────┐          │            │
│      │  │   Metrics (OpenTelemetry)  │          │            │
│      │  └────────────┬────────────────┘          │            │
│      │               │                           │            │
│      │  ┌────────────▼────────────────┐          │            │
│      │  │   Docker Client (restart)   │          │            │
│      │  └────────────┬────────────────┘          │            │
│      └───────────────┼────────────────────────────┘            │
│                      │         │                              │
│                      │ OTLP    │ Docker Socket                │
│                      │ :4317   │ (container restart)          │
│                      │         │                              │
│      ┌───────────────▼─────────▼──────────────┐               │
│      │   OTel Collector Container             │               │
│      │   - OTLP Receiver (:4317)              │               │
│      │   - Batch Processor                    │               │
│      │   - Logging Exporter                   │               │
│      │   - Prometheus Exporter (:8889)        │               │
│      │   - Health Check (:13133)              │               │
│      │   - Config: /etc/otel-collector-config │               │
│      └───────────────┬────────────────────────┘               │
│                      │                                        │
│                      ▼                                        │
│              Prometheus :8889/metrics ←─ External scrape     │
│              (or other exporters)                            │
└──────────────────────────────────────────────────────────────┘
```

## File Structure

```
slo-metric-generator/
├── main.go                           # Application entry point and server setup
├── handlers.go                       # API endpoint handlers with failure simulation
├── admin.go                          # Admin UI HTTP handlers and configuration API
├── loadgen.go                        # Built-in load generation engine
├── metrics.go                        # OpenTelemetry setup and metric recording
├── config.go                         # Thread-safe configuration store
├── otelconfig.go                     # OTel Collector config management and Docker integration
├── static/
│   └── admin.html                    # Web UI with Monaco editor for OTel config
├── configs/
│   ├── otel-collector-default.yaml   # Default collector configuration template
│   └── otel-collector.yaml           # Active collector config (git-ignored)
├── Dockerfile                        # Multi-stage Docker build
├── docker-compose.yml                # Complete stack orchestration
├── .dockerignore                     # Docker build exclusions
├── .gitignore                        # Git exclusions
├── go.mod                            # Go module dependencies
└── README.md                         # This file
```

## Development

### Running Tests

```bash
go test ./...
```

### Building Binary

```bash
go build -o slo-metric-generator
./slo-metric-generator
```

## Docker Deployment

### Complete Stack with Docker Compose (Recommended)

The Docker Compose setup provides a complete, self-contained observability stack:

**Services included:**
- **SLO Metric Generator** - Main application with admin UI
- **OpenTelemetry Collector** - Metrics collection and export
- **Prometheus Server** - Time-series database with web UI and querying
- **Prometheus Exporter** - Built-in Prometheus endpoint for metrics

**Start the stack:**

```bash
# Build and start all services
docker-compose up --build

# Run in background
docker-compose up -d --build

# View logs
docker-compose logs -f

# Stop the stack
docker-compose down
```

**Access the services:**
- **Admin UI**: http://localhost:8080/admin
- **API Endpoints**: http://localhost:8080/api/*
- **Prometheus UI**: http://localhost:9090
- **Prometheus Metrics (OTel)**: http://localhost:8889/metrics
- **Collector Health**: http://localhost:13133

**Architecture:**

```
┌─────────────────────────────────────────────────────────────┐
│  Docker Compose Stack                                       │
│                                                             │
│  ┌──────────────────┐         ┌─────────────────────────┐  │
│  │  SLO Generator   │  OTLP   │  OTel Collector         │  │
│  │  :8080           │────────>│  :4317 (OTLP)           │  │
│  │                  │         │  :8889 (Prometheus)     │  │
│  │  - Admin UI      │         │  :13133 (Health)        │  │
│  │  - API Endpoints │         │                         │  │
│  │  - Config Editor │<────────│  - Batch Processor      │  │
│  │  - Load Gen      │ Restart │  - Logging Exporter     │  │
│  └──────────────────┘         │  - Prometheus Exporter  │  │
│         │                     └─────────────────────────┘  │
│         │ Docker Socket (restart collector)                │
│         ▼                                                   │
│  /var/run/docker.sock                                      │
└─────────────────────────────────────────────────────────────┘
```

### Configuration Persistence

The collector configuration is stored in `configs/otel-collector.yaml` and persists across container restarts:

```bash
# View current config
cat configs/otel-collector.yaml

# Reset to default
cp configs/otel-collector-default.yaml configs/otel-collector.yaml
docker-compose restart otel-collector
```

### Customizing the Deployment

**Environment Variables:**

Edit `docker-compose.yml` to customize:

```yaml
services:
  app:
    environment:
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
      - OTEL_COLLECTOR_CONTAINER=otel-collector
```

**Ports:**

Change exposed ports in `docker-compose.yml`:

```yaml
services:
  app:
    ports:
      - "8080:8080"  # Change left side to use different host port

  otel-collector:
    ports:
      - "8889:8889"  # Prometheus metrics
```

**Collector Image Version:**

Update the collector version in `docker-compose.yml`:

```yaml
services:
  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.96.0  # Change version here
```

### Standalone Docker (No Collector)

If you only want to run the application without the bundled collector:

```bash
# Build the image
docker build -t slo-metric-generator .

# Run with external collector
docker run -p 8080:8080 \
  -e OTEL_EXPORTER_OTLP_ENDPOINT="your-collector:4317" \
  slo-metric-generator
```

### Docker Network Configuration

To connect to an external collector on the host:

```bash
# Linux: Use host network mode
docker run --network host slo-metric-generator

# Mac/Windows: Use host.docker.internal
docker run -p 8080:8080 \
  -e OTEL_EXPORTER_OTLP_ENDPOINT="host.docker.internal:4317" \
  slo-metric-generator
```

### Troubleshooting Docker Deployment

**Collector not restarting from UI:**
- Check Docker socket is mounted: `docker inspect slo-metric-generator | grep docker.sock`
- Check app logs: `docker-compose logs app`
- Verify container name matches `OTEL_COLLECTOR_CONTAINER` env var

**Metrics not flowing:**
- Check collector health: `curl http://localhost:13133`
- Verify OTLP endpoint: `docker-compose logs app | grep OTLP`
- Check collector logs: `docker-compose logs otel-collector`

**Configuration changes not persisting:**
- Ensure `configs/` directory is volume-mounted
- Check file permissions: `ls -la configs/`
- Verify `configs/otel-collector.yaml` exists

**Port conflicts:**
- If ports are in use, edit `docker-compose.yml` and change the left side of port mappings
- Example: `"8081:8080"` to use port 8081 on host

## Troubleshooting

### Metrics not appearing in collector

1. Verify OTLP endpoint is correct: `echo $OTEL_EXPORTER_OTLP_ENDPOINT`
2. Check collector is running and accessible
3. Look for connection errors in application logs
4. Ensure firewall allows gRPC traffic on port 4317

### Configuration changes not applying

1. Check browser console for errors
2. Verify rates sum to 100% (check validation indicator)
3. Check application logs for configuration update messages

### High error rates not reflected in metrics

1. Generate more traffic (small sample sizes may not match exact percentages)
2. Wait for metrics export interval (10 seconds)
3. Check OTLP collector receives metrics

## License

MIT License - feel free to use this for testing and development purposes.
