// main.go — Gin + OpenTelemetry (OTLP/HTTP → Tempo) with slog logging
// and a sync.Map-based in-memory store.

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

/* -------------------------------------------------------------------------- */
/* Types & globals                                                            */
/* -------------------------------------------------------------------------- */

type Item struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

var (
	store sync.Map     // map[int]Item — thread-safe
	idSeq atomic.Int64 // auto-incrementing ID
)

/* -------------------------------------------------------------------------- */
/* OpenTelemetry setup                                                        */
/* -------------------------------------------------------------------------- */

func initOpenTelemetry() func() {
	ctx := context.Background()

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")), // e.g. "tempo:4318"
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		panic("failed to create OTLP-HTTP exporter: " + err.Error())
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("otel-crud-example"),
		)),
	)
	otel.SetTracerProvider(tp)

	return func() { _ = tp.Shutdown(ctx) }
}

/* -------------------------------------------------------------------------- */
/* slog middleware (status + trace ID)                                        */
/* -------------------------------------------------------------------------- */

func slogWithTrace(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		span := trace.SpanFromContext(c.Request.Context())
		l.Info("request",
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", c.Writer.Status(),
			"trace_id", span.SpanContext().TraceID().String(),
		)
	}
}

/* -------------------------------------------------------------------------- */
/* Main & routes                                                              */
/* -------------------------------------------------------------------------- */

func main() {
	shutdown := initOpenTelemetry()
	defer shutdown()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	r := gin.New()
	r.Use(otelgin.Middleware("otel-crud-example"))
	r.Use(slogWithTrace(logger))

	r.POST("/items", createItem)
	r.GET("/items", listItems)
	r.GET("/items/:id", getItem)
	r.PUT("/items/:id", updateItem)
	r.DELETE("/items/:id", deleteItem)

	logger.Info("Listening on :8080 …")
	if err := r.Run(":8080"); err != nil {
		logger.Error("server error", "err", err)
	}
}

/* -------------------------------------------------------------------------- */
/* CRUD handlers                                                              */
/* -------------------------------------------------------------------------- */

func createItem(c *gin.Context) {
	var in struct{ Name string }
	if err := c.ShouldBindJSON(&in); err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}

	id := int(idSeq.Add(1))
	item := Item{ID: id, Name: in.Name}
	store.Store(id, item)

	c.JSON(http.StatusCreated, item)
}

func listItems(c *gin.Context) {
	items := make([]Item, 0)
	store.Range(func(_, v any) bool {
		items = append(items, v.(Item))
		return true
	})
	c.JSON(http.StatusOK, items)
}

func getItem(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}
	val, ok := store.Load(id)
	if !ok {
		respondError(c, errors.New("not found"), http.StatusNotFound)
		return
	}
	c.JSON(http.StatusOK, val.(Item))
}

func updateItem(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}
	val, ok := store.Load(id)
	if !ok {
		respondError(c, errors.New("not found"), http.StatusNotFound)
		return
	}
	item := val.(Item)

	var in struct{ Name string }
	if err := c.ShouldBindJSON(&in); err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}

	item.Name = in.Name
	store.Store(id, item)
	c.JSON(http.StatusOK, item)
}

func deleteItem(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}
	if _, ok := store.Load(id); !ok {
		respondError(c, errors.New("not found"), http.StatusNotFound)
		return
	}
	store.Delete(id)
	c.Status(http.StatusNoContent)
}

/* -------------------------------------------------------------------------- */
/* Error helper                                                               */
/* -------------------------------------------------------------------------- */

func respondError(c *gin.Context, err error, status int) {
	span := traceSpan(c.Request.Context())
	span.RecordError(err, trace.WithAttributes(attribute.String("http.status_text", http.StatusText(status))))
	span.SetStatus(codes.Error, err.Error())

	c.JSON(status, gin.H{"error": err.Error()})
}

/* -------------------------------------------------------------------------- */
/* Span helper                                                                */
/* -------------------------------------------------------------------------- */

func traceSpan(ctx context.Context) trace.Span {
	if span := trace.SpanFromContext(ctx); span != nil {
		return span
	}
	return trace.SpanFromContext(context.Background())
}
