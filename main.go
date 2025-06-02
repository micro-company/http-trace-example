// main.go — Gin CRUD demo with:
//   • OpenTelemetry traces exported via OTLP/HTTP → Tempo
//   • sync.Map store (thread-safe)
//   • slog structured logging (trace-ID + status)
//   • Spec-compliant error handling per
//     https://opentelemetry.io/docs/specs/otel/error-handling/

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
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
	idSeq atomic.Int64 // monotonic ID
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
/* slog middleware (adds trace-ID + status)                                   */
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
/* Recovery middleware → spec-compliant panic handling                        */
/* -------------------------------------------------------------------------- */

func recoveryWithOtel(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				err := fmt.Errorf("panic: %v", rec)

				span := traceSpan(c.Request.Context())
				span.RecordError(err,
					trace.WithStackTrace(true),
					trace.WithAttributes(
						attribute.Bool("exception.escaped", true),
						attribute.String("exception.stacktrace", string(debug.Stack())),
					),
				)
				span.SetStatus(codes.Error, "panic")

				l.Error("panic recovered",
					"error", err,
					"trace_id", span.SpanContext().TraceID().String(),
				)
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

/* -------------------------------------------------------------------------- */
/* Main                                                                       */
/* -------------------------------------------------------------------------- */

func main() {
	shutdown := initOpenTelemetry()
	defer shutdown()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	r := gin.New() // no default logger/recovery
	r.Use(otelgin.Middleware("otel-crud-example"))
	r.Use(recoveryWithOtel(logger))
	r.Use(slogWithTrace(logger))

	// CRUD routes
	r.POST("/items", createItem)
	r.GET("/items", listItems)
	r.GET("/items/:id", getItem)
	r.PUT("/items/:id", updateItem)
	r.DELETE("/items/:id", deleteItem)

	/* 500-error demos */
	r.GET("/fail", func(c *gin.Context) {
		respondError(c, errors.New("simulated server failure"), http.StatusInternalServerError)
	})
	r.GET("/panic", func(_ *gin.Context) {
		panic("simulated panic")
	})

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
/* Error helper (spec-compliant)                                              */
/* -------------------------------------------------------------------------- */

func respondError(c *gin.Context, err error, status int) {
	span := traceSpan(c.Request.Context())

	// always record the error event
	span.RecordError(err)

	// mark span failed only for 5xx (server-side) errors
	if status >= 500 {
		span.SetStatus(codes.Error, err.Error())
	}

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
