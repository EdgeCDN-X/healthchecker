#!/bin/bash
docker run --rm --name otel-collector -p 4318:4318 -v "$(pwd)/../otel-collector-config.yaml:/etc/otelcol/config.yaml" otel/opentelemetry-collector:latest --config=/etc/otelcol/config.yaml