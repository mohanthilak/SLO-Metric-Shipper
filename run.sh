#!/bin/bash

# SLO Metric Generator - Quick Start Script

# Set default OTLP endpoint if not already set
if [ -z "$OTEL_EXPORTER_OTLP_ENDPOINT" ]; then
    export OTEL_EXPORTER_OTLP_ENDPOINT="localhost:4317"
    echo "Using default OTLP endpoint: localhost:4317"
fi

echo "========================================="
echo "  SLO Metric Generator"
echo "========================================="
echo ""
echo "OTLP Endpoint: $OTEL_EXPORTER_OTLP_ENDPOINT"
echo ""
echo "Starting server..."
echo ""

# Run the application
go run .
