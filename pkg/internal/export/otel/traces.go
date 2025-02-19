package otel

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mariomac/pipes/pipe"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/exporter/otlphttpexporter"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.19.0"
	trace2 "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/grafana/beyla/pkg/internal/export/attributes"
	attr "github.com/grafana/beyla/pkg/internal/export/attributes/names"
	"github.com/grafana/beyla/pkg/internal/imetrics"
	"github.com/grafana/beyla/pkg/internal/pipe/global"
	"github.com/grafana/beyla/pkg/internal/request"
)

func tlog() *slog.Logger {
	return slog.With("component", "otel.TracesReporter")
}

const reporterName = "github.com/grafana/beyla"

type TracesConfig struct {
	CommonEndpoint string `yaml:"-" env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	TracesEndpoint string `yaml:"endpoint" env:"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"`

	Protocol       Protocol `yaml:"protocol" env:"OTEL_EXPORTER_OTLP_PROTOCOL"`
	TracesProtocol Protocol `yaml:"-" env:"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"`

	// InsecureSkipVerify is not standard, so we don't follow the same naming convention
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" env:"BEYLA_OTEL_INSECURE_SKIP_VERIFY"`

	Sampler Sampler `yaml:"sampler"`

	// Configuration options below this line will remain undocumented at the moment,
	// but can be useful for performance-tuning of some customers.
	MaxExportBatchSize int           `yaml:"max_export_batch_size" env:"BEYLA_OTLP_TRACES_MAX_EXPORT_BATCH_SIZE"`
	MaxQueueSize       int           `yaml:"max_queue_size" env:"BEYLA_OTLP_TRACES_MAX_QUEUE_SIZE"`
	BatchTimeout       time.Duration `yaml:"batch_timeout" env:"BEYLA_OTLP_TRACES_BATCH_TIMEOUT"`
	ExportTimeout      time.Duration `yaml:"export_timeout" env:"BEYLA_OTLP_TRACES_EXPORT_TIMEOUT"`

	ReportersCacheLen int `yaml:"reporters_cache_len" env:"BEYLA_TRACES_REPORT_CACHE_LEN"`

	// SDKLogLevel works independently from the global LogLevel because it prints GBs of logs in Debug mode
	// and the Info messages leak internal details that are not usually valuable for the final user.
	SDKLogLevel string `yaml:"otel_sdk_log_level" env:"BEYLA_OTEL_SDK_LOG_LEVEL"`

	// Grafana configuration needs to be explicitly set up before building the graph
	Grafana *GrafanaOTLP `yaml:"-"`
}

// Enabled specifies that the OTEL traces node is enabled if and only if
// either the OTEL endpoint and OTEL traces endpoint is defined.
// If not enabled, this node won't be instantiated
func (m TracesConfig) Enabled() bool { //nolint:gocritic
	return m.CommonEndpoint != "" || m.TracesEndpoint != "" || m.Grafana.TracesEnabled()
}

func (m *TracesConfig) getProtocol() Protocol {
	if m.TracesProtocol != "" {
		return m.TracesProtocol
	}
	if m.Protocol != "" {
		return m.Protocol
	}
	return m.guessProtocol()
}

func (m *TracesConfig) guessProtocol() Protocol {
	// If no explicit protocol is set, we guess it it from the metrics enpdoint port
	// (assuming it uses a standard port or a development-like form like 14317, 24317, 14318...)
	ep, _, err := parseTracesEndpoint(m)
	if err == nil {
		if strings.HasSuffix(ep.Port(), UsualPortGRPC) {
			return ProtocolGRPC
		} else if strings.HasSuffix(ep.Port(), UsualPortHTTP) {
			return ProtocolHTTPProtobuf
		}
	}
	// Otherwise we return default protocol according to the latest specification:
	// https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/protocol/exporter.md?plain=1#L53
	return ProtocolHTTPProtobuf
}

// TracesReceiver creates a terminal node that consumes request.Spans and sends OpenTelemetry metrics to the configured consumers.
func TracesReceiver(ctx context.Context, cfg TracesConfig, ctxInfo *global.ContextInfo, userAttribSelection attributes.Selection) pipe.FinalProvider[[]request.Span] {
	return (&tracesOTELReceiver{ctx: ctx, cfg: cfg, ctxInfo: ctxInfo, attributes: userAttribSelection}).provideLoop
}

type tracesOTELReceiver struct {
	ctx        context.Context
	cfg        TracesConfig
	ctxInfo    *global.ContextInfo
	attributes attributes.Selection
}

func GetUserSelectedAttributes(attrs attributes.Selection) (map[attr.Name]struct{}, error) {
	// Get user attributes
	attribProvider, err := attributes.NewAttrSelector(attributes.GroupTraces, attrs)
	if err != nil {
		return nil, err
	}
	traceAttrsArr := attribProvider.For(attributes.Traces)
	traceAttrs := make(map[attr.Name]struct{})
	for _, a := range traceAttrsArr {
		traceAttrs[a] = struct{}{}
	}

	return traceAttrs, err
}

func (tr *tracesOTELReceiver) provideLoop() (pipe.FinalFunc[[]request.Span], error) {
	if !tr.cfg.Enabled() {
		return pipe.IgnoreFinal[[]request.Span](), nil
	}
	return func(in <-chan []request.Span) {
		exp, err := getTracesExporter(tr.ctx, tr.cfg, tr.ctxInfo)
		if err != nil {
			slog.Error("error creating traces exporter", "error", err)
			return
		}
		defer func() {
			err := exp.Shutdown(tr.ctx)
			if err != nil {
				slog.Error("error shutting down traces exporter", "error", err)
			}
		}()
		err = exp.Start(tr.ctx, nil)
		if err != nil {
			slog.Error("error starting traces exporter", "error", err)
			return
		}

		traceAttrs, err := GetUserSelectedAttributes(tr.attributes)
		if err != nil {
			slog.Error("error selecting user trace attributes", "error", err)
			return
		}

		for spans := range in {
			for i := range spans {
				span := &spans[i]
				if span.IgnoreSpan == request.IgnoreTraces {
					continue
				}
				traces := GenerateTraces(span, traceAttrs)
				err := exp.ConsumeTraces(tr.ctx, traces)
				if err != nil {
					slog.Error("error sending trace to consumer", "error", err)
				}
			}
		}
	}, nil
}

func getTracesExporter(ctx context.Context, cfg TracesConfig, ctxInfo *global.ContextInfo) (exporter.Traces, error) {
	switch proto := cfg.getProtocol(); proto {
	case ProtocolHTTPJSON, ProtocolHTTPProtobuf, "": // zero value defaults to HTTP for backwards-compatibility
		slog.Debug("instantiating HTTP TracesReporter", "protocol", proto)
		var t trace.SpanExporter
		var err error

		opts, err := getHTTPTracesEndpointOptions(&cfg)
		if err != nil {
			slog.Error("can't get HTTP traces endpoint options", "error", err)
			return nil, err
		}
		if t, err = httpTracer(ctx, opts); err != nil {
			slog.Error("can't instantiate OTEL HTTP traces exporter", err)
			return nil, err
		}
		endpoint, _, err := parseTracesEndpoint(&cfg)
		if err != nil {
			slog.Error("can't parse traces endpoint", "error", err)
			return nil, err
		}
		factory := otlphttpexporter.NewFactory()
		config := factory.CreateDefaultConfig().(*otlphttpexporter.Config)
		config.QueueConfig.Enabled = false
		config.ClientConfig = confighttp.ClientConfig{
			Endpoint: endpoint.String(),
			TLSSetting: configtls.ClientConfig{
				Insecure:           opts.Insecure,
				InsecureSkipVerify: cfg.InsecureSkipVerify,
			},
			Headers: convertHeaders(opts.HTTPHeaders),
		}
		set := getTraceSettings(ctxInfo, cfg, t)
		return factory.CreateTracesExporter(ctx, set, config)
	case ProtocolGRPC:
		slog.Debug("instantiating GRPC TracesReporter", "protocol", proto)
		var t trace.SpanExporter
		var err error
		opts, err := getGRPCTracesEndpointOptions(&cfg)
		if err != nil {
			slog.Error("can't get GRPC traces endpoint options", "error", err)
			return nil, err
		}
		if t, err = grpcTracer(ctx, opts); err != nil {
			slog.Error("can't instantiate OTEL GRPC traces exporter: %w", err)
			return nil, err
		}
		endpoint, _, err := parseTracesEndpoint(&cfg)
		if err != nil {
			slog.Error("can't parse GRPC traces endpoint", "error", err)
			return nil, err
		}
		factory := otlpexporter.NewFactory()
		config := factory.CreateDefaultConfig().(*otlpexporter.Config)
		config.QueueConfig.Enabled = false
		config.ClientConfig = configgrpc.ClientConfig{
			Endpoint: endpoint.String(),
			TLSSetting: configtls.ClientConfig{
				Insecure:           opts.Insecure,
				InsecureSkipVerify: cfg.InsecureSkipVerify,
			},
		}
		set := getTraceSettings(ctxInfo, cfg, t)
		return factory.CreateTracesExporter(ctx, set, config)
	default:
		slog.Error(fmt.Sprintf("invalid protocol value: %q. Accepted values are: %s, %s, %s",
			proto, ProtocolGRPC, ProtocolHTTPJSON, ProtocolHTTPProtobuf))
		return nil, fmt.Errorf("invalid protocol value: %q", proto)
	}

}

func getTraceSettings(ctxInfo *global.ContextInfo, cfg TracesConfig, in trace.SpanExporter) exporter.CreateSettings {
	var opts []trace.BatchSpanProcessorOption
	if cfg.MaxExportBatchSize > 0 {
		opts = append(opts, trace.WithMaxExportBatchSize(cfg.MaxExportBatchSize))
	}
	if cfg.MaxQueueSize > 0 {
		opts = append(opts, trace.WithMaxQueueSize(cfg.MaxQueueSize))
	}
	if cfg.BatchTimeout > 0 {
		opts = append(opts, trace.WithBatchTimeout(cfg.BatchTimeout))
	}
	if cfg.ExportTimeout > 0 {
		opts = append(opts, trace.WithExportTimeout(cfg.ExportTimeout))
	}
	tracer := instrumentTraceExporter(in, ctxInfo.Metrics)
	bsp := trace.NewBatchSpanProcessor(tracer, opts...)
	provider := trace.NewTracerProvider(
		trace.WithSpanProcessor(bsp),
		trace.WithSampler(cfg.Sampler.Implementation()),
	)
	telemetrySettings := component.TelemetrySettings{
		Logger:         zap.NewNop(),
		MeterProvider:  metric.NewMeterProvider(),
		TracerProvider: provider,
		MetricsLevel:   configtelemetry.LevelBasic,
		ReportStatus: func(event *component.StatusEvent) {
			if err := event.Err(); err != nil {
				slog.Error("error reported by component", "error", err)
			}
		},
	}
	return exporter.CreateSettings{
		ID:                component.NewIDWithName(component.DataTypeMetrics, "beyla"),
		TelemetrySettings: telemetrySettings,
	}
}

// GenerateTraces creates a ptrace.Traces from a request.Span
func GenerateTraces(span *request.Span, userAttrs map[attr.Name]struct{}) ptrace.Traces {
	t := span.Timings()
	start := spanStartTime(t)
	hasSubSpans := t.Start.After(start)
	traces := ptrace.NewTraces()
	rs := traces.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	resourceAttrs := attrsToMap(getResourceAttrs(span.ServiceID).Attributes())
	resourceAttrs.PutStr(string(semconv.OTelLibraryNameKey), reporterName)
	resourceAttrs.CopyTo(rs.Resource().Attributes())

	traceID := pcommon.TraceID(span.TraceID)
	spanID := pcommon.SpanID(randomSpanID())
	if traceID.IsEmpty() {
		traceID = pcommon.TraceID(randomTraceID())
	}

	if hasSubSpans {
		createSubSpans(span, spanID, traceID, &ss, t)
	} else if span.SpanID.IsValid() {
		spanID = pcommon.SpanID(span.SpanID)
	}

	// Create a parent span for the whole request session
	s := ss.Spans().AppendEmpty()
	s.SetName(TraceName(span))
	s.SetKind(ptrace.SpanKind(spanKind(span)))
	s.SetStartTimestamp(pcommon.NewTimestampFromTime(start))

	// Set trace and span IDs
	s.SetSpanID(spanID)
	s.SetTraceID(traceID)
	if span.ParentSpanID.IsValid() {
		s.SetParentSpanID(pcommon.SpanID(span.ParentSpanID))
	}

	// Set span attributes
	attrs := traceAttributes(span, userAttrs)
	m := attrsToMap(attrs)
	m.CopyTo(s.Attributes())

	// Set status code
	statusCode := codeToStatusCode(SpanStatusCode(span))
	s.Status().SetCode(statusCode)
	s.SetEndTimestamp(pcommon.NewTimestampFromTime(t.End))
	return traces
}

// createSubSpans creates the internal spans for a request.Span
func createSubSpans(span *request.Span, parentSpanID pcommon.SpanID, traceID pcommon.TraceID, ss *ptrace.ScopeSpans, t request.Timings) {
	// Create a child span showing the queue time
	spQ := ss.Spans().AppendEmpty()
	spQ.SetName("in queue")
	spQ.SetStartTimestamp(pcommon.NewTimestampFromTime(t.RequestStart))
	spQ.SetKind(ptrace.SpanKindInternal)
	spQ.SetEndTimestamp(pcommon.NewTimestampFromTime(t.Start))
	spQ.SetTraceID(traceID)
	spQ.SetSpanID(pcommon.SpanID(randomSpanID()))
	spQ.SetParentSpanID(parentSpanID)

	// Create a child span showing the processing time
	spP := ss.Spans().AppendEmpty()
	spP.SetName("processing")
	spP.SetStartTimestamp(pcommon.NewTimestampFromTime(t.Start))
	spP.SetKind(ptrace.SpanKindInternal)
	spP.SetEndTimestamp(pcommon.NewTimestampFromTime(t.End))
	spP.SetTraceID(traceID)
	if span.SpanID.IsValid() {
		spP.SetSpanID(pcommon.SpanID(span.SpanID))
	} else {
		spP.SetSpanID(pcommon.SpanID(randomSpanID()))
	}
	spP.SetParentSpanID(parentSpanID)
}

// attrsToMap converts a slice of attribute.KeyValue to a pcommon.Map
func attrsToMap(attrs []attribute.KeyValue) pcommon.Map {
	m := pcommon.NewMap()
	for _, attr := range attrs {
		switch v := attr.Value.AsInterface().(type) {
		case string:
			m.PutStr(string(attr.Key), v)
		case int64:
			m.PutInt(string(attr.Key), v)
		case float64:
			m.PutDouble(string(attr.Key), v)
		case bool:
			m.PutBool(string(attr.Key), v)
		}
	}
	return m
}

// codeToStatusCode converts a codes.Code to a ptrace.StatusCode
func codeToStatusCode(code codes.Code) ptrace.StatusCode {
	switch code {
	case codes.Unset:
		return ptrace.StatusCodeUnset
	case codes.Error:
		return ptrace.StatusCodeError
	case codes.Ok:
		return ptrace.StatusCodeOk
	}
	return ptrace.StatusCodeUnset
}

func convertHeaders(headers map[string]string) map[string]configopaque.String {
	opaqueHeaders := make(map[string]configopaque.String)
	for key, value := range headers {
		opaqueHeaders[key] = configopaque.String(value)
	}
	return opaqueHeaders
}

func httpTracer(ctx context.Context, opts otlpOptions) (*otlptrace.Exporter, error) {
	texp, err := otlptracehttp.New(ctx, opts.AsTraceHTTP()...)
	if err != nil {
		return nil, fmt.Errorf("creating HTTP trace exporter: %w", err)
	}
	return texp, nil
}

func grpcTracer(ctx context.Context, opts otlpOptions) (*otlptrace.Exporter, error) {
	texp, err := otlptracegrpc.New(ctx, opts.AsTraceGRPC()...)
	if err != nil {
		return nil, fmt.Errorf("creating GRPC trace exporter: %w", err)
	}
	return texp, nil
}

// instrumentTraceExporter checks whether the context is configured to report internal metrics and,
// in this case, wraps the passed traces exporter inside an instrumented exporter
func instrumentTraceExporter(in trace.SpanExporter, internalMetrics imetrics.Reporter) trace.SpanExporter {
	// avoid wrapping the instrumented exporter if we don't have
	// internal instrumentation (NoopReporter)
	if _, ok := internalMetrics.(imetrics.NoopReporter); ok || internalMetrics == nil {
		return in
	}
	return &instrumentedTracesExporter{
		SpanExporter: in,
		internal:     internalMetrics,
	}
}

// https://opentelemetry.io/docs/specs/otel/trace/semantic_conventions/http/#status
func httpSpanStatusCode(span *request.Span) codes.Code {
	if span.Status < 400 {
		return codes.Unset
	}

	if span.Status < 500 {
		if span.Type == request.EventTypeHTTPClient {
			return codes.Error
		}
		return codes.Unset
	}

	return codes.Error
}

// https://opentelemetry.io/docs/specs/otel/trace/semantic_conventions/rpc/#grpc-status
func grpcSpanStatusCode(span *request.Span) codes.Code {
	if span.Type == request.EventTypeGRPCClient {
		if span.Status == int(semconv.RPCGRPCStatusCodeOk.Value.AsInt64()) {
			return codes.Unset
		}
		return codes.Error
	}

	switch int64(span.Status) {
	case semconv.RPCGRPCStatusCodeUnknown.Value.AsInt64(),
		semconv.RPCGRPCStatusCodeDeadlineExceeded.Value.AsInt64(),
		semconv.RPCGRPCStatusCodeUnimplemented.Value.AsInt64(),
		semconv.RPCGRPCStatusCodeInternal.Value.AsInt64(),
		semconv.RPCGRPCStatusCodeUnavailable.Value.AsInt64(),
		semconv.RPCGRPCStatusCodeDataLoss.Value.AsInt64():
		return codes.Error
	}

	return codes.Unset
}

func SpanStatusCode(span *request.Span) codes.Code {
	switch span.Type {
	case request.EventTypeHTTP, request.EventTypeHTTPClient:
		return httpSpanStatusCode(span)
	case request.EventTypeGRPC, request.EventTypeGRPCClient:
		return grpcSpanStatusCode(span)
	case request.EventTypeSQLClient:
		if span.Status != 0 {
			return codes.Error
		}
		return codes.Unset
	}
	return codes.Unset
}

func SpanKindString(span *request.Span) string {
	switch span.Type {
	case request.EventTypeHTTP, request.EventTypeGRPC:
		return "SPAN_KIND_SERVER"
	case request.EventTypeHTTPClient, request.EventTypeGRPCClient, request.EventTypeSQLClient:
		return "SPAN_KIND_CLIENT"
	}
	return "SPAN_KIND_INTERNAL"
}

func SpanHost(span *request.Span) string {
	if span.HostName != "" {
		return span.HostName
	}

	return span.Host
}

func SpanPeer(span *request.Span) string {
	if span.PeerName != "" {
		return span.PeerName
	}

	return span.Peer
}

func traceAttributes(span *request.Span, optionalAttrs map[attr.Name]struct{}) []attribute.KeyValue {
	var attrs []attribute.KeyValue

	switch span.Type {
	case request.EventTypeHTTP:
		attrs = []attribute.KeyValue{
			request.HTTPRequestMethod(span.Method),
			request.HTTPResponseStatusCode(span.Status),
			request.HTTPUrlPath(span.Path),
			request.ClientAddr(request.SpanPeer(span)),
			request.ServerAddr(request.SpanHost(span)),
			request.ServerPort(span.HostPort),
			request.HTTPRequestBodySize(int(span.ContentLength)),
		}
		if span.Route != "" {
			attrs = append(attrs, semconv.HTTPRoute(span.Route))
		}
	case request.EventTypeGRPC:
		attrs = []attribute.KeyValue{
			semconv.RPCMethod(span.Path),
			semconv.RPCSystemGRPC,
			semconv.RPCGRPCStatusCodeKey.Int(span.Status),
			request.ClientAddr(request.SpanPeer(span)),
			request.ServerAddr(request.SpanHost(span)),
			request.ServerPort(span.HostPort),
		}
	case request.EventTypeHTTPClient:
		attrs = []attribute.KeyValue{
			request.HTTPRequestMethod(span.Method),
			request.HTTPResponseStatusCode(span.Status),
			request.HTTPUrlFull(span.Path),
			request.ServerAddr(request.SpanHost(span)),
			request.ServerPort(span.HostPort),
			request.HTTPRequestBodySize(int(span.ContentLength)),
		}
	case request.EventTypeGRPCClient:
		attrs = []attribute.KeyValue{
			semconv.RPCMethod(span.Path),
			semconv.RPCSystemGRPC,
			semconv.RPCGRPCStatusCodeKey.Int(span.Status),
			request.ServerAddr(request.SpanHost(span)),
			request.ServerPort(span.HostPort),
		}
	case request.EventTypeSQLClient:
		if _, ok := optionalAttrs[attr.IncludeDBStatement]; ok {
			attrs = append(attrs, semconv.DBStatement(span.Statement))
		}
		operation := span.Method
		if operation != "" {
			attrs = append(attrs, semconv.DBOperation(operation))
			table := span.Path
			if table != "" {
				attrs = append(attrs, semconv.DBSQLTable(table))
			}
		}
	}

	return attrs
}

func TraceName(span *request.Span) string {
	switch span.Type {
	case request.EventTypeHTTP:
		name := span.Method
		if span.Route != "" {
			name += " " + span.Route
		}
		return name
	case request.EventTypeGRPC, request.EventTypeGRPCClient:
		return span.Path
	case request.EventTypeHTTPClient:
		return span.Method
	case request.EventTypeSQLClient:
		// We don't have db.name, but follow "<db.operation> <db.name>.<db.sql.table_name>"
		// or just "<db.operation>" if table is not known, otherwise just a fixed string.
		operation := span.Method
		if operation == "" {
			return "SQL"
		}
		table := span.Path
		if table != "" {
			operation += " ." + table
		}
		return operation
	}
	return ""
}

func spanKind(span *request.Span) trace2.SpanKind {
	switch span.Type {
	case request.EventTypeHTTP, request.EventTypeGRPC:
		return trace2.SpanKindServer
	case request.EventTypeHTTPClient, request.EventTypeGRPCClient, request.EventTypeSQLClient:
		return trace2.SpanKindClient
	}
	return trace2.SpanKindInternal
}

func spanStartTime(t request.Timings) time.Time {
	realStart := t.RequestStart
	if t.Start.Before(realStart) {
		realStart = t.Start
	}
	return realStart
}

// the HTTP path will be defined from one of the following sources, from highest to lowest priority
// - OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, if defined
// - OTEL_EXPORTER_OTLP_ENDPOINT, if defined
// - https://otlp-gateway-${GRAFANA_CLOUD_ZONE}.grafana.net/otlp, if GRAFANA_CLOUD_ZONE is defined
// If, by some reason, Grafana changes its OTLP Gateway URL in a distant future, you can still point to the
// correct URL with the OTLP_EXPORTER_... variables.
func parseTracesEndpoint(cfg *TracesConfig) (*url.URL, bool, error) {
	isCommon := false
	endpoint := cfg.TracesEndpoint
	if endpoint == "" {
		isCommon = true
		endpoint = cfg.CommonEndpoint
		if endpoint == "" && cfg.Grafana != nil && cfg.Grafana.CloudZone != "" {
			endpoint = cfg.Grafana.Endpoint()
		}
	}

	murl, err := url.Parse(endpoint)
	if err != nil {
		return nil, isCommon, fmt.Errorf("parsing endpoint URL %s: %w", endpoint, err)
	}
	if murl.Scheme == "" || murl.Host == "" {
		return nil, isCommon, fmt.Errorf("URL %q must have a scheme and a host", endpoint)
	}
	return murl, isCommon, nil
}

func getHTTPTracesEndpointOptions(cfg *TracesConfig) (otlpOptions, error) {
	opts := otlpOptions{}
	log := tlog().With("transport", "http")

	murl, isCommon, err := parseTracesEndpoint(cfg)
	if err != nil {
		return opts, err
	}

	log.Debug("Configuring exporter", "protocol",
		cfg.Protocol, "tracesProtocol", cfg.TracesProtocol, "endpoint", murl.Host)
	setTracesProtocol(cfg)
	opts.Endpoint = murl.Host
	if murl.Scheme == "http" || murl.Scheme == "unix" {
		log.Debug("Specifying insecure connection", "scheme", murl.Scheme)
		opts.Insecure = true
	}
	// If the value is set from the OTEL_EXPORTER_OTLP_ENDPOINT common property, we need to add /v1/traces to the path
	// otherwise, we leave the path that is explicitly set by the user
	opts.URLPath = murl.Path
	if isCommon {
		if strings.HasSuffix(opts.URLPath, "/") {
			opts.URLPath += "v1/traces"
		} else {
			opts.URLPath += "/v1/traces"
		}
		log.Debug("Specifying path", "path", opts.URLPath)
	}

	if cfg.InsecureSkipVerify {
		log.Debug("Setting InsecureSkipVerify")
		opts.SkipTLSVerify = true
	}

	cfg.Grafana.setupOptions(&opts)

	return opts, nil
}

func getGRPCTracesEndpointOptions(cfg *TracesConfig) (otlpOptions, error) {
	opts := otlpOptions{}
	log := tlog().With("transport", "grpc")
	murl, _, err := parseTracesEndpoint(cfg)
	if err != nil {
		return opts, err
	}

	log.Debug("Configuring exporter", "protocol",
		cfg.Protocol, "tracesProtocol", cfg.TracesProtocol, "endpoint", murl.Host)
	opts.Endpoint = murl.Host
	if murl.Scheme == "http" || murl.Scheme == "unix" {
		log.Debug("Specifying insecure connection", "scheme", murl.Scheme)
		opts.Insecure = true
	}

	if cfg.InsecureSkipVerify {
		log.Debug("Setting InsecureSkipVerify")
		opts.SkipTLSVerify = true
	}

	return opts, nil
}

// HACK: at the time of writing this, the otelptracehttp API does not support explicitly
// setting the protocol. They should be properly set via environment variables, but
// if the user supplied the value via configuration file (and not via env vars), we override the environment.
// To be as least intrusive as possible, we will change the variables if strictly needed
// TODO: remove this once otelptracehttp.WithProtocol is supported
func setTracesProtocol(cfg *TracesConfig) {
	if _, ok := os.LookupEnv(envTracesProtocol); ok {
		return
	}
	if _, ok := os.LookupEnv(envProtocol); ok {
		return
	}
	if cfg.TracesProtocol != "" {
		os.Setenv(envTracesProtocol, string(cfg.TracesProtocol))
		return
	}
	if cfg.Protocol != "" {
		os.Setenv(envProtocol, string(cfg.Protocol))
		return
	}
	// unset. Guessing it
	os.Setenv(envTracesProtocol, string(cfg.guessProtocol()))
}
