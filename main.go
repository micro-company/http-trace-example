// main.go — Gin version with OpenTelemetry (OTLP/HTTP → Tempo) and
// structured logs that include the trace ID and HTTP status.

package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"

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
	store   = make(map[int]Item)
	idSeq   = 0
	storeMu sync.RWMutex
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
/* Logging middleware (status + trace ID)                                     */
/* -------------------------------------------------------------------------- */

func logWithTrace() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next() // run handler chain

		span := trace.SpanFromContext(c.Request.Context())
		traceID := span.SpanContext().TraceID().String()

		log.Printf("method=%s path=%s status=%d trace_id=%s",
			c.Request.Method, c.FullPath(), c.Writer.Status(), traceID)
	}
}

/* -------------------------------------------------------------------------- */
/* Main & routes                                                              */
/* -------------------------------------------------------------------------- */

func main() {
	shutdown := initOpenTelemetry()
	defer shutdown()

	r := gin.New()
	r.Use(otelgin.Middleware("otel-crud-example")) // creates spans
	r.Use(logWithTrace())                          // logs with trace IDs

	// CRUD routes
	r.POST("/items", createItem)
	r.GET("/items", listItems)
	r.GET("/items/:id", getItem)
	r.PUT("/items/:id", updateItem)
	r.DELETE("/items/:id", deleteItem)

	log.Println("Listening on :8080 …")
	if err := r.Run(":8080"); err != nil {
		log.Fatalf("server error: %v", err)
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

	storeMu.Lock()
	idSeq++
	item := Item{ID: idSeq, Name: in.Name}
	store[item.ID] = item
	storeMu.Unlock()

	c.JSON(http.StatusCreated, item)
}

func listItems(c *gin.Context) {
	storeMu.RLock()
	items := make([]Item, 0, len(store))
	for _, it := range store {
		items = append(items, it)
	}
	storeMu.RUnlock()

	c.JSON(http.StatusOK, items)
}

func getItem(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}

	storeMu.RLock()
	item, ok := store[id]
	storeMu.RUnlock()
	if !ok {
		respondError(c, errors.New("not found"), http.StatusNotFound)
		return
	}
	c.JSON(http.StatusOK, item)
}

func updateItem(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}

	storeMu.Lock()
	item, ok := store[id]
	storeMu.Unlock()
	if !ok {
		respondError(c, errors.New("not found"), http.StatusNotFound)
		return
	}

	var in struct{ Name string }
	if err := c.ShouldBindJSON(&in); err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}

	item.Name = in.Name
	storeMu.Lock()
	store[id] = item
	storeMu.Unlock()

	c.JSON(http.StatusOK, item)
}

func deleteItem(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondError(c, err, http.StatusBadRequest)
		return
	}

	storeMu.Lock()
	_, ok := store[id]
	if ok {
		delete(store, id)
	}
	storeMu.Unlock()

	if !ok {
		respondError(c, errors.New("not found"), http.StatusNotFound)
		return
	}
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
