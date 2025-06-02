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

// Item represents a simple entity stored in memory.
type Item struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// store is an in‑memory thread‑safe map acting as a fake database.
var (
	store   = make(map[int]Item)
	idSeq   = 0
	storeMu sync.RWMutex
)

func main() {
	shutdown := initOpenTelemetry()
	defer shutdown()

	mux := http.NewServeMux()
	mux.Handle("/items", otelhttp.NewHandler(http.HandlerFunc(itemsHandler), "itemsCollection"))
	mux.Handle("/items/", otelhttp.NewHandler(http.HandlerFunc(itemHandler), "singleItem"))

	log.Println("Listening on :8080 …")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// initOpenTelemetry configures an OTLP-HTTP exporter for Tempo
// and falls back to stdout if it can't be created.
func initOpenTelemetry() func() {
	ctx := context.Background()

	httpExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")), // e.g. "tempo:4318"
		otlptracehttp.WithInsecure(),                                         // Tempo’s HTTP receiver is plain-text
	)
	if err != nil {
		panic("failed to create OTLP HTTP exporter: " + err.Error())
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

// itemsHandler implements POST /items and GET /items.
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

// itemHandler implements GET, PUT, DELETE /items/{id}.
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

// createItem adds a new item to the store.
func createItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		Name string `json:"name"`
	}
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

// listItems returns all items.
func listItems(w http.ResponseWriter, r *http.Request) {
	storeMu.RLock()
	defer storeMu.RUnlock()

	items := make([]Item, 0, len(store))
	for _, it := range store {
		items = append(items, it)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// getItem returns a single item.
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

// updateItem changes an item’s name.
func updateItem(ctx context.Context, w http.ResponseWriter, r *http.Request, id int) {
	storeMu.Lock()
	item, ok := store[id]
	storeMu.Unlock()
	if !ok {
		respondError(ctx, w, errors.New("not found"), http.StatusNotFound)
		return
	}

	var in struct {
		Name string `json:"name"`
	}
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

// deleteItem removes an item.
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

// respondError records err on the current span, sets status = Error, and writes an HTTP error.
func respondError(ctx context.Context, w http.ResponseWriter, err error, status int) {
	span := traceSpan(ctx)
	span.RecordError(err, trace.WithAttributes(attribute.String("http.status_text", http.StatusText(status))))
	span.SetStatus(codes.Error, err.Error())

	http.Error(w, err.Error(), status)
}

// traceSpan returns the current span from ctx or a Noop span if none exists.
func traceSpan(ctx context.Context) trace.Span {
	if span := trace.SpanFromContext(ctx); span != nil {
		return span
	}
	return trace.SpanFromContext(context.Background())
}
