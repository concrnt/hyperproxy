package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/labstack/echo-contrib/echoprometheus"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/labstack/echo/otelecho"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
)

var denyIps = []string{
	"127.0.0.0/8",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"::1/128",
	"fc00::/7",
}

var (
	mc     *memcache.Client
	client = &http.Client{
		Timeout: 10 * time.Second,
	}
	tracer = otel.Tracer("hyperproxy")
)

const (
	useragent = "hyperproxy bot"
)

func main() {

	mc = memcache.New(os.Getenv("MEMCACHED_HOST"))
	defer mc.Close()

	e := echo.New()
	e.Use(middleware.Recover())

	traceEndpoint := os.Getenv("HYPERPROXY_TRACE_ENDPOINT")
	if traceEndpoint != "" {
		cleanup, err := setupTraceProvider(traceEndpoint, "hyperproxy", "")
		if err != nil {
			panic(err)
		}
		defer cleanup()

		skipper := otelecho.WithSkipper(
			func(c echo.Context) bool {
				return c.Path() == "/metrics" || c.Path() == "/health"
			},
		)
		e.Use(otelecho.Middleware("hyperproxy", skipper))
	}

	e.Use(echoprometheus.NewMiddlewareWithConfig(echoprometheus.MiddlewareConfig{
		Namespace: "hyperproxy",
		LabelFuncs: map[string]echoprometheus.LabelValueFunc{
			"url": func(c echo.Context, err error) string {
				return "REDACTED"
			},
		},
		Skipper: func(c echo.Context) bool {
			return c.Path() == "/metrics" || c.Path() == "/health"
		},
	}))

	init_resize(512 * 1024 * 1024)

	sem := make(chan struct{}, 1)

	e.GET("/image/*", func(c echo.Context) error {
		sem <- struct{}{}
		defer func() {
			<-sem
		}()

		return ImageHandler(c)
	})

	e.GET("/summary", SummaryHandler)

	var currentCacheSizeMetrics = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hyperproxy_image_cache_size",
		Help: "Current size of the image cache",
	})
	prometheus.MustRegister(currentCacheSizeMetrics)

	go func() {
		totalsize := CleanDiskCache()
		currentCacheSizeMetrics.Set(float64(totalsize))

		ticker := time.NewTicker(5 * time.Minute)
		for {
			select {
			case <-ticker.C:
				totalsize := CleanDiskCache()
				currentCacheSizeMetrics.Set(float64(totalsize))
			}
		}
	}()

	e.GET("/metrics", echoprometheus.NewHandler())

	PORT := os.Getenv("PORT")
	if PORT == "" {
		PORT = "8080"
	}

	e.Logger.Fatal(e.Start(":" + PORT))
}

func setupTraceProvider(endpoint string, serviceName string, serviceVersion string) (func(), error) {

	exporter, err := otlptracehttp.New(
		context.Background(),
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)

	if err != nil {
		return nil, err
	}

	resource := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(serviceName),
		semconv.ServiceVersionKey.String(serviceVersion),
	)

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource),
	)
	otel.SetTracerProvider(tracerProvider)

	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(propagator)

	cleanup := func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := tracerProvider.Shutdown(ctx); err != nil {
			fmt.Println(fmt.Sprintf("Failed to shutdown tracer provider: %v", err))
		}
	}
	return cleanup, nil
}
