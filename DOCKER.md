# Docker Deployment Guide

Complete guide for running SLO Metric Generator with Docker and Docker Compose.

## Quick Start

```bash
# Start the complete stack
docker-compose up -d

# View logs
docker-compose logs -f

# Stop the stack
docker-compose down
```

## Services Overview

The Docker Compose stack includes:

### 1. SLO Metric Generator (app)
- **Port**: 8080
- **Purpose**: Main application with API endpoints and admin UI
- **Access**: http://localhost:8080/admin

### 2. OpenTelemetry Collector (otel-collector)
- **Ports**:
  - 4317: OTLP gRPC receiver (metrics ingestion)
  - 8889: Prometheus metrics exporter
  - 13133: Health check endpoint
- **Purpose**: Collects and exports metrics
- **Config**: `configs/otel-collector.yaml`

### 3. Prometheus Server (prometheus)
- **Port**: 9090
- **Purpose**: Time-series database for metrics storage and querying
- **Data Storage**: `prometheus-data/` directory (persistent)
- **Config**: `prometheus.yml`

## Accessing Services

| Service | URL | Description |
|---------|-----|-------------|
| Admin UI | http://localhost:8080/admin | Web interface for configuration |
| API Endpoints | http://localhost:8080/api/* | Simulated API endpoints |
| Prometheus UI | http://localhost:9090 | Prometheus web interface |
| Prometheus Graph | http://localhost:9090/graph | Query and visualize metrics |
| Prometheus Targets | http://localhost:9090/targets | Scrape target status |
| Prometheus Metrics (OTel) | http://localhost:8889/metrics | Raw metrics from collector |
| Collector Health | http://localhost:13133 | Health check |

## Using the OTel Config Editor

The admin UI includes a built-in editor for the OpenTelemetry Collector configuration:

### Features
- **Monaco Editor**: Professional code editor with YAML syntax highlighting
- **Real-time Validation**: Checks YAML syntax before saving
- **Automatic Restart**: Collector restarts automatically when configuration is saved
- **Safe Editing**: Backup created before each change

### How to Edit Configuration

1. Open http://localhost:8080/admin in your browser
2. Scroll to "OpenTelemetry Collector Configuration" section
3. Edit the YAML configuration in the Monaco editor
4. Click "Save & Restart Collector"
5. Wait 2-3 seconds for the collector to restart

### Example: Adding a New Exporter

Add Prometheus Remote Write to send metrics to Grafana Cloud:

```yaml
exporters:
  # Existing exporters
  logging:
    loglevel: info
  prometheus:
    endpoint: "0.0.0.0:8889"
    namespace: slo

  # Add new exporter
  prometheusremotewrite:
    endpoint: https://prometheus-us-central1.grafana.net/api/prom/push
    headers:
      authorization: Basic YOUR_BASE64_CREDENTIALS

service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [logging, prometheus, prometheusremotewrite]  # Add to pipeline
```

## Configuration Management

### Config File Locations

- **Default template**: `configs/otel-collector-default.yaml` (version controlled)
- **Active config**: `configs/otel-collector.yaml` (git-ignored, persists across restarts)
- **Backup**: `configs/otel-collector.yaml.backup` (created before each save)

### Resetting Configuration

To reset to default configuration:

```bash
# Copy default config over active config
cp configs/otel-collector-default.yaml configs/otel-collector.yaml

# Restart the collector
docker-compose restart otel-collector
```

### Manual Configuration

You can also edit the config file directly:

```bash
# Edit the active config
nano configs/otel-collector.yaml

# Restart collector to apply changes
docker-compose restart otel-collector
```

## Environment Variables

Customize the deployment by editing `docker-compose.yml`:

```yaml
services:
  app:
    environment:
      # OTLP endpoint for metrics export
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317

      # Container name for Docker API operations
      - OTEL_COLLECTOR_CONTAINER=otel-collector

      # Optional: OTLP headers for authentication
      # - OTEL_EXPORTER_OTLP_HEADERS=x-api-key=your-key
```

## Port Configuration

Change ports by editing `docker-compose.yml`:

```yaml
services:
  app:
    ports:
      - "8080:8080"  # Change left side: "8081:8080" for port 8081

  otel-collector:
    ports:
      - "4317:4317"  # OTLP gRPC
      - "8889:8889"  # Prometheus
      - "13133:13133"  # Health check
```

## Volume Mounts

The stack uses the following volume mounts:

```yaml
volumes:
  # Docker socket for container restart (read-only)
  - /var/run/docker.sock:/var/run/docker.sock:ro

  # Config persistence
  - ./configs:/app/configs
```

## Networking

Containers communicate via Docker bridge network `slo-network`:

```
┌─────────────────────────────┐
│  slo-network (bridge)       │
│                             │
│  ┌───────────────────────┐  │
│  │  slo-metric-generator │  │
│  │  IP: auto-assigned    │  │
│  └──────────┬────────────┘  │
│             │ OTLP :4317    │
│  ┌──────────▼────────────┐  │
│  │  otel-collector       │  │
│  │  IP: auto-assigned    │  │
│  └───────────────────────┘  │
└─────────────────────────────┘
```

## Troubleshooting

### Collector Not Restarting from UI

**Problem**: Config changes don't restart the collector

**Solutions**:
1. Check Docker socket is mounted:
   ```bash
   docker inspect slo-metric-generator | grep docker.sock
   ```

2. Check app logs for errors:
   ```bash
   docker-compose logs app | grep -i docker
   ```

3. Verify container name matches environment variable:
   ```bash
   docker ps | grep otel-collector
   ```

### Metrics Not Flowing

**Problem**: Metrics not appearing in Prometheus endpoint

**Solutions**:
1. Check collector health:
   ```bash
   curl http://localhost:13133
   ```

2. Verify OTLP connection:
   ```bash
   docker-compose logs app | grep OTLP
   ```

3. Check collector logs:
   ```bash
   docker-compose logs otel-collector | tail -50
   ```

4. Test the metrics endpoint:
   ```bash
   curl http://localhost:8889/metrics
   ```

### Configuration Not Persisting

**Problem**: Config changes lost after restart

**Solutions**:
1. Ensure configs directory exists:
   ```bash
   ls -la configs/
   ```

2. Check volume mount:
   ```bash
   docker inspect slo-metric-generator | grep -A5 Mounts
   ```

3. Verify file permissions:
   ```bash
   ls -la configs/otel-collector.yaml
   ```

### Port Already in Use

**Problem**: `Error: port is already allocated`

**Solutions**:
1. Find process using the port:
   ```bash
   lsof -i :8080  # or :8889, :4317, etc.
   ```

2. Change port in docker-compose.yml:
   ```yaml
   ports:
     - "8081:8080"  # Use 8081 on host instead
   ```

### Container Fails to Start

**Problem**: Container exits immediately

**Solutions**:
1. Check logs:
   ```bash
   docker-compose logs app
   docker-compose logs otel-collector
   ```

2. Verify config syntax:
   ```bash
   yamllint configs/otel-collector.yaml
   ```

3. Restart the stack:
   ```bash
   docker-compose down
   docker-compose up -d
   ```

## Advanced Usage

### Running with External Collector

If you want to use an external collector instead:

```bash
# Stop the bundled collector
docker-compose stop otel-collector

# Update app environment
docker-compose exec app sh -c 'export OTEL_EXPORTER_OTLP_ENDPOINT=external-collector:4317'

# Or edit docker-compose.yml and restart
```

### Connecting to Host Services (Mac/Windows)

To connect to services on your host machine:

```yaml
environment:
  # Use special DNS name for host
  - OTEL_EXPORTER_OTLP_ENDPOINT=host.docker.internal:4317
```

### Scaling for High Load

For production or high-load testing:

```yaml
services:
  app:
    deploy:
      replicas: 3  # Run multiple instances
      resources:
        limits:
          cpus: '2'
          memory: 1G

  otel-collector:
    deploy:
      resources:
        limits:
          cpus: '1'
          memory: 512M
```

### Using Different Collector Version

To use a different OpenTelemetry Collector version:

```yaml
services:
  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.100.0  # Change version
```

Available versions: https://hub.docker.com/r/otel/opentelemetry-collector-contrib/tags

## Security Considerations

### Docker Socket Access

The app container has read-only access to the Docker socket for restarting the collector:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock:ro  # Read-only
```

**Security Notes**:
- Read-only minimizes security risk
- Only used for container restart operations
- For production, consider using a Docker socket proxy

### Config File Permissions

Default permissions for config files:

```bash
# Config files should be readable by all, writable by owner
chmod 644 configs/otel-collector.yaml
```

### Credentials in Config

**Never commit credentials to git!**

For production deployments:
1. Use environment variables for secrets
2. Mount secrets as files
3. Use a secrets manager

Example with environment variables:

```yaml
exporters:
  prometheusremotewrite:
    endpoint: ${PROM_ENDPOINT}
    headers:
      authorization: Basic ${PROM_CREDENTIALS}
```

## Maintenance

### Viewing Logs

```bash
# All logs
docker-compose logs -f

# Specific service
docker-compose logs -f app
docker-compose logs -f otel-collector

# Last 100 lines
docker-compose logs --tail=100
```

### Updating Images

```bash
# Pull latest images
docker-compose pull

# Rebuild app
docker-compose build --no-cache app

# Restart with new images
docker-compose up -d
```

### Cleaning Up

```bash
# Stop and remove containers
docker-compose down

# Remove containers and volumes
docker-compose down -v

# Remove images
docker-compose down --rmi all
```

## Using Prometheus

### Accessing Prometheus UI

1. Open http://localhost:9090 in your browser
2. Navigate to Graph or Explore to query metrics
3. Check Targets (http://localhost:9090/targets) to verify scraping is working

### Sample PromQL Queries

**Total requests by endpoint:**
```promql
sum by (endpoint) (slo_http_requests_total)
```

**Success rate (percentage):**
```promql
sum(rate(slo_http_requests_total{status_code="200"}[5m])) /
sum(rate(slo_http_requests_total[5m])) * 100
```

**Request rate per second:**
```promql
rate(slo_http_requests_total[1m])
```

**Error rate by status code:**
```promql
sum by (status_code) (rate(slo_http_requests_total{status_code!="200"}[5m]))
```

**SLO Availability (99.9% target):**
```promql
100 - (
  sum(rate(slo_http_requests_total{status_code=~"5.."}[5m])) /
  sum(rate(slo_http_requests_total[5m])) * 100
)
```

### Persistent Data Storage

Prometheus data is stored in `./prometheus-data/` directory:

```bash
# View data size
du -sh prometheus-data/

# Backup data
tar -czf prometheus-backup-$(date +%Y%m%d).tar.gz prometheus-data/

# Restore from backup
tar -xzf prometheus-backup-20261505.tar.gz

# Clean old data (requires restart)
docker-compose down
rm -rf prometheus-data/*
docker-compose up -d
```

### Prometheus Configuration

Edit `prometheus.yml` to modify scrape configs:

```yaml
global:
  scrape_interval: 15s      # How often to scrape targets
  evaluation_interval: 15s  # How often to evaluate rules

scrape_configs:
  - job_name: 'slo-metrics'
    static_configs:
      - targets: ['otel-collector:8889']
    # Optional: add scrape interval override
    scrape_interval: 10s
```

After modifying `prometheus.yml`, reload config:

```bash
# Hot reload (no restart needed)
curl -X POST http://localhost:9090/-/reload

# Or restart the container
docker-compose restart prometheus
```

## Integration Examples

### With Grafana

1. Add Prometheus data source pointing to `http://localhost:8889`
2. Create dashboards using `slo_http_requests_total` metric

### With Grafana Cloud

Edit OTel config through UI or manually:

```yaml
exporters:
  prometheusremotewrite:
    endpoint: https://prometheus-REGION.grafana.net/api/prom/push
    headers:
      authorization: Basic BASE64_ENCODED_CREDENTIALS
```

## Additional Resources

- [OpenTelemetry Collector Documentation](https://opentelemetry.io/docs/collector/)
- [OTLP Protocol Specification](https://opentelemetry.io/docs/specs/otlp/)
- [Prometheus Exposition Formats](https://prometheus.io/docs/instrumenting/exposition_formats/)
- [Docker Compose Documentation](https://docs.docker.com/compose/)
