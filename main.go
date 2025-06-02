package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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

	httpExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")), // e.g. "tempo:4318"
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		panic("failed to create OTLP-HTTP exporter: " + err.Error())
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(httpExp),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("otel-crud-example"),
		)),
	)
	otel.SetTracerProvider(tp)

	return func() { _ = tp.Shutdown(ctx) }
}

/* -------------------------------------------------------------------------- */
/* Logging middleware (adds traceID + status)                                 */
/* -------------------------------------------------------------------------- */

// statusRecorder captures the status code written by the handler.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// loggingMiddleware logs method, path, status, and traceID for each request.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Serve the request (otelhttp creates the span inside).
		next.ServeHTTP(sr, r)

		span := trace.SpanFromContext(r.Context())
		traceID := span.SpanContext().TraceID().String()

		log.Printf("method=%s path=%s status=%d trace_id=%s",
			r.Method, r.URL.Path, sr.status, traceID)
	})
}

/* -------------------------------------------------------------------------- */
/* Main & routes                                                              */
/* -------------------------------------------------------------------------- */

func main() {
	shutdown := initOpenTelemetry()
	defer shutdown()

	mux := http.NewServeMux()
	mux.Handle("/items",
		otelhttp.NewHandler(
			loggingMiddleware(http.HandlerFunc(itemsHandler)),
			"itemsCollection",
		),
	)
	mux.Handle("/items/",
		otelhttp.NewHandler(
			loggingMiddleware(http.HandlerFunc(itemHandler)),
			"singleItem",
		),
	)

	log.Println("Listening on :8080 …")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

/* -------------------------------------------------------------------------- */
/* Handlers                                                                   */
/* -------------------------------------------------------------------------- */

func itemsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		createItem(w, r)
	case http.MethodGet:
		listItems(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func itemHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[len("/items/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		respondError(r.Context(), w, fmt.Errorf("invalid id: %w", err), http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		getItem(r.Context(), w, id)
	case http.MethodPut:
		updateItem(r.Context(), w, r, id)
	case http.MethodDelete:
		deleteItem(r.Context(), w, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

/* -------------------------------------------------------------------------- */
/* CRUD helpers                                                               */
/* -------------------------------------------------------------------------- */

func createItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(ctx, w, err, http.StatusBadRequest)
		return
	}

	storeMu.Lock()
	idSeq++
	item := Item{ID: idSeq, Name: in.Name}
	store[item.ID] = item
	storeMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(item)
}

func listItems(w http.ResponseWriter, r *http.Request) {
	storeMu.RLock()
	items := make([]Item, 0, len(store))
	for _, it := range store {
		items = append(items, it)
	}
	storeMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

func getItem(ctx context.Context, w http.ResponseWriter, id int) {
	storeMu.RLock()
	item, ok := store[id]
	storeMu.RUnlock()
	if !ok {
		respondError(ctx, w, errors.New("not found"), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(item)
}

func updateItem(ctx context.Context, w http.ResponseWriter, r *http.Request, id int) {
	storeMu.Lock()
	item, ok := store[id]
	storeMu.Unlock()
	if !ok {
		respondError(ctx, w, errors.New("not found"), http.StatusNotFound)
		return
	}

	var in struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(ctx, w, err, http.StatusBadRequest)
		return
	}

	item.Name = in.Name
	storeMu.Lock()
	store[id] = item
	storeMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(item)
}

func deleteItem(ctx context.Context, w http.ResponseWriter, id int) {
	storeMu.Lock()
	_, ok := store[id]
	if ok {
		delete(store, id)
	}
	storeMu.Unlock()

	if !ok {
		respondError(ctx, w, errors.New("not found"), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* -------------------------------------------------------------------------- */
/* Error helper                                                               */
/* -------------------------------------------------------------------------- */

func respondError(ctx context.Context, w http.ResponseWriter, err error, status int) {
	span := traceSpan(ctx)
	span.RecordError(err, trace.WithAttributes(attribute.String("http.status_text", http.StatusText(status))))
	span.SetStatus(codes.Error, err.Error())

	http.Error(w, err.Error(), status)
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
