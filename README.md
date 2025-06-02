## opentelemetry example

### Getting started

```
docker compose up -d
export OTEL_EXPORTER_OTLP_ENDPOINT=127.0.0.1:4318
go run ./main.go
bash ./demo.sh
```