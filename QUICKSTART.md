# Quick Start Guide

## 🚀 Running the Application

### Option 1: Using the Binary
```bash
./slo-metric-generator
```

### Option 2: Using the Quick Start Script
```bash
./run.sh
```

### Option 3: Using Go Run
```bash
go run .
```

## 🌐 Accessing the Admin UI

Once the server is running, open your browser to:

**http://localhost:8080/admin**

## 📊 Using the Admin UI

### 1. Configure Status Code Distribution

For each endpoint, you'll see three sliders:
- **Success (2xx)** - Green slider for successful responses
- **Client Error (4xx)** - Yellow slider for client errors (400, 404, 429)
- **Server Error (5xx)** - Red slider for server errors (500, 503)

The sliders automatically adjust to ensure they always total 100%.

### 2. Generate Load

For each endpoint, configure:
1. **Requests/Second**: How many requests per second to send (1-1000)
2. **Total Requests**: Total number of requests (0 = unlimited/continuous)
3. Click **Start** to begin generating traffic
4. Click **Stop** to stop

### 3. Monitor Real-Time Statistics

While load generation is running, you'll see live updates:
- Total Sent
- Success Count (2xx)
- Client Error Count (4xx)
- Server Error Count (5xx)
- Elapsed Time
- Success Rate %

## 🎯 Example Workflow

1. **Start the application**
   ```bash
   ./run.sh
   ```

2. **Open the Admin UI**
   - Navigate to http://localhost:8080/admin

3. **Configure an endpoint** (e.g., "users")
   - Set: 80% Success, 15% Client Error, 5% Server Error

4. **Start load generation**
   - Requests/Second: 50
   - Total Requests: 0 (unlimited)
   - Click "Start"

5. **Watch the statistics**
   - You'll see requests being sent
   - Success rate should stabilize around 80%
   - Errors distributed according to your configuration

6. **Check your observability platform**
   - Metrics are exported via OTLP to your configured endpoint
   - View the data in your SLO dashboard

## 🔧 Configuration

### OTLP Endpoint

Set the OpenTelemetry collector endpoint:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="your-collector:4317"
./run.sh
```

### With Authentication

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="your-collector:4317"
export OTEL_EXPORTER_OTLP_HEADERS="x-api-key=your-api-key"
./run.sh
```

## 📈 Testing SLO Scenarios

### High Availability Test (99.9%)
1. Set all endpoints: 99.9% success, 0.1% server error
2. Start load: 10 req/sec, unlimited
3. Verify ~99.9% success rate in real-time stats
4. Check SLO dashboard for availability metrics

### Degraded Service Test
1. Set "checkout": 70% success, 20% 4xx, 10% 5xx
2. Set other endpoints: 99% success, 1% errors
3. Start load on all endpoints
4. Compare degraded vs healthy endpoints in dashboard

### Load Testing
1. Configure desired error rates
2. Start high load: 100+ req/sec per endpoint
3. Monitor how your observability platform handles high volume
4. Test dashboard query performance

## 🛠️ Troubleshooting

### Can't access Admin UI
- Verify the server is running: `curl http://localhost:8080/api/health`
- Check port 8080 is not in use by another application

### Metrics not appearing
- Verify OTLP endpoint is correct
- Check your OpenTelemetry collector is running
- Look for connection errors in server logs

### Load generation not working
- Check browser console for JavaScript errors
- Verify status code percentages sum to 100%
- Ensure requests/second is between 1-1000

## 📊 API Endpoints

The following API endpoints are available for direct testing:

- `GET http://localhost:8080/api/users`
- `GET http://localhost:8080/api/products`
- `GET http://localhost:8080/api/orders`
- `GET http://localhost:8080/api/checkout`
- `GET http://localhost:8080/api/health`

## 🎨 Features

✅ Real-time status code distribution configuration
✅ Built-in load generation (no external tools needed)
✅ Live statistics and monitoring
✅ Multiple status codes (200, 400, 404, 429, 500, 503)
✅ OpenTelemetry metrics export
✅ No restart required for configuration changes
✅ Thread-safe concurrent request handling

## 📚 Next Steps

1. Configure your OpenTelemetry collector
2. Set up your SLO dashboard
3. Use the built-in load generator to simulate traffic
4. Test different failure scenarios
5. Validate your SLO calculations and alerting

For more details, see [README.md](README.md)
