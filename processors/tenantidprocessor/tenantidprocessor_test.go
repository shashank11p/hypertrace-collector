package tenantidprocessor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"testing"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/jaegertracing/jaeger/model"
	jaegerconvert "github.com/jaegertracing/jaeger/model/converter/thrift/jaeger"
	jaegerthrift "github.com/jaegertracing/jaeger/thrift-gen/jaeger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/receiver/jaegerreceiver"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/collector/testutil"
	"go.opentelemetry.io/collector/translator/trace/jaeger"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const testTenantID = "jdoe"

func TestMissingMetadataInContext(t *testing.T) {
	p := &processor{
		logger:               zap.NewNop(),
		tenantIDHeaderName:   defaultHeaderName,
		tenantIDAttributeKey: defaultHeaderName,
	}
	_, err := p.ProcessTraces(context.Background(), pdata.NewTraces())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not extract headers")
}

func TestMissingTenantHeader(t *testing.T) {
	p := &processor{
		logger:               zap.NewNop(),
		tenantIDHeaderName:   defaultHeaderName,
		tenantIDAttributeKey: defaultHeaderName,
	}

	md := metadata.New(map[string]string{})
	ctx := metadata.NewIncomingContext(
		context.Background(),
		md,
	)
	_, err := p.ProcessTraces(ctx, pdata.NewTraces())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing header")
}

func TestMultipleTenantHeaders(t *testing.T) {
	p := &processor{
		logger:               zap.NewNop(),
		tenantIDHeaderName:   defaultHeaderName,
		tenantIDAttributeKey: defaultHeaderName,
	}

	md := metadata.New(map[string]string{p.tenantIDHeaderName: testTenantID})
	md.Append(p.tenantIDHeaderName, "jdoe2")
	ctx := metadata.NewIncomingContext(
		context.Background(),
		md,
	)
	_, err := p.ProcessTraces(ctx, pdata.NewTraces())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple tenant ID headers")
}

func TestEmptyTraces(t *testing.T) {
	p := &processor{
		logger:               zap.NewNop(),
		tenantIDHeaderName:   defaultHeaderName,
		tenantIDAttributeKey: defaultHeaderName,
	}
	traces := pdata.NewTraces()
	md := metadata.New(map[string]string{p.tenantIDHeaderName: testTenantID})
	ctx := metadata.NewIncomingContext(
		context.Background(),
		md,
	)
	gotTraces, err := p.ProcessTraces(ctx, traces)
	require.NoError(t, err)
	assert.Equal(t, traces, gotTraces)
}

func TestReceiveOTLPGRPC(t *testing.T) {
	sink := new(consumertest.TracesSink)
	tenantProcessor := &processor{
		logger:               zap.NewNop(),
		tenantIDHeaderName:   defaultHeaderName,
		tenantIDAttributeKey: defaultAttributeKey,
	}

	addr := testutil.GetAvailableLocalAddress(t)
	factory := otlpreceiver.NewFactory()
	cfg := factory.CreateDefaultConfig().(*otlpreceiver.Config)
	cfg.GRPC.NetAddr.Endpoint = addr
	cfg.HTTP = nil
	params := component.ReceiverCreateParams{Logger: zap.NewNop()}
	otlpRec, err := factory.CreateTracesReceiver(context.Background(), params, cfg, multiConsumer{
		sink:              sink,
		tenantIDprocessor: tenantProcessor,
	})
	require.NoError(t, err)
	err = otlpRec.Start(context.Background(), componenttest.NewNopHost())
	require.NoError(t, err)
	defer otlpRec.Shutdown(context.Background())

	conn, err := grpc.Dial(cfg.GRPC.NetAddr.Endpoint, grpc.WithInsecure())
	require.NoError(t, err)
	defer conn.Close()

	otlpExpFac := otlpexporter.NewFactory()
	exporter, err := otlpExpFac.CreateTracesExporter(
		context.Background(),
		component.ExporterCreateParams{Logger: zap.NewNop()},
		&otlpexporter.Config{
			GRPCClientSettings: configgrpc.GRPCClientSettings{
				Headers:      map[string]string{tenantProcessor.tenantIDHeaderName: testTenantID},
				Endpoint:     addr,
				WaitForReady: true,
				TLSSetting: configtls.TLSClientSetting{
					Insecure: true,
				},
			},
		},
	)
	require.NoError(t, err)
	reqTraces := GenerateTraceDataOneSpan()
	err = exporter.ConsumeTraces(context.Background(), reqTraces)
	require.NoError(t, err)

	traces := sink.AllTraces()
	assert.Equal(t, 1, len(traces))
	tenantAttrsFound := assertTenantAttributeExists(
		t,
		traces[0],
		tenantProcessor.tenantIDAttributeKey,
		testTenantID,
	)
	assert.Equal(t, reqTraces.SpanCount(), tenantAttrsFound)
}

func TestReceiveJaegerThriftHTTP(t *testing.T) {
	sink := new(consumertest.TracesSink)
	tenantProcessor := &processor{
		logger:               zap.NewNop(),
		tenantIDHeaderName:   defaultHeaderName,
		tenantIDAttributeKey: defaultAttributeKey,
	}

	addr := testutil.GetAvailableLocalAddress(t)
	cfg := &jaegerreceiver.Config{
		Protocols: jaegerreceiver.Protocols{
			ThriftHTTP: &confighttp.HTTPServerSettings{
				Endpoint: addr,
			},
		},
	}
	params := component.ReceiverCreateParams{Logger: zap.NewNop()}
	jrf := jaegerreceiver.NewFactory()
	rec, err := jrf.CreateTracesReceiver(context.Background(), params, cfg, multiConsumer{
		sink:              sink,
		tenantIDprocessor: tenantProcessor,
	})
	require.NoError(t, err)

	err = rec.Start(context.Background(), componenttest.NewNopHost())
	require.NoError(t, err)
	defer rec.Shutdown(context.Background())

	td := GenerateTraceDataOneSpan()
	batches, err := jaeger.InternalTracesToJaegerProto(td)
	require.NoError(t, err)
	collectorAddr := fmt.Sprintf("http://%s/api/traces", addr)
	for _, batch := range batches {
		err := sendToJaegerHTTPThrift(collectorAddr, map[string]string{tenantProcessor.tenantIDHeaderName: testTenantID}, jaegerModelToThrift(batch))
		require.NoError(t, err)
	}

	traces := sink.AllTraces()
	assert.Equal(t, 1, len(traces))
	tenantAttrsFound := assertTenantAttributeExists(
		t,
		traces[0],
		tenantProcessor.tenantIDAttributeKey,
		testTenantID,
	)
	assert.Equal(t, td.SpanCount(), tenantAttrsFound)
}

func assertTenantAttributeExists(t *testing.T, trace pdata.Traces, tenantAttrKey string, tenantID string) int {
	numOfTenantAttrs := 0
	rss := trace.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)

		ilss := rs.InstrumentationLibrarySpans()
		for j := 0; j < ilss.Len(); j++ {
			ils := ilss.At(j)

			spans := ils.Spans()
			for k := 0; k < spans.Len(); k++ {
				span := spans.At(k)
				tenantAttr, ok := span.Attributes().Get(tenantAttrKey)
				require.True(t, ok)
				numOfTenantAttrs++
				assert.Equal(t, pdata.AttributeValueSTRING, tenantAttr.Type())
				assert.Equal(t, tenantID, tenantAttr.StringVal())
			}
		}
	}
	return numOfTenantAttrs
}

type multiConsumer struct {
	sink              *consumertest.TracesSink
	tenantIDprocessor *processor
}

var _ consumer.TracesConsumer = (*multiConsumer)(nil)

func (f multiConsumer) ConsumeTraces(ctx context.Context, td pdata.Traces) error {
	traces, err := f.tenantIDprocessor.ProcessTraces(ctx, td)
	if err != nil {
		return err
	}
	return f.sink.ConsumeTraces(ctx, traces)
}

var (
	resourceAttributes1    = map[string]pdata.AttributeValue{"resource-attr": pdata.NewAttributeValueString("resource-attr-val-1")}
	TestSpanStartTime      = time.Date(2020, 2, 11, 20, 26, 12, 321, time.UTC)
	TestSpanStartTimestamp = pdata.TimestampUnixNano(TestSpanStartTime.UnixNano())
	TestSpanEventTime      = time.Date(2020, 2, 11, 20, 26, 13, 123, time.UTC)
	TestSpanEventTimestamp = pdata.TimestampUnixNano(TestSpanEventTime.UnixNano())

	TestSpanEndTime      = time.Date(2020, 2, 11, 20, 26, 13, 789, time.UTC)
	TestSpanEndTimestamp = pdata.TimestampUnixNano(TestSpanEndTime.UnixNano())
	spanEventAttributes  = map[string]pdata.AttributeValue{"span-event-attr": pdata.NewAttributeValueString("span-event-attr-val")}
)

func GenerateTraceDataOneSpan() pdata.Traces {
	td := GenerateTraceDataOneEmptyInstrumentationLibrary()
	rs0ils0 := td.ResourceSpans().At(0).InstrumentationLibrarySpans().At(0)
	rs0ils0.Spans().Resize(1)
	fillSpanOne(rs0ils0.Spans().At(0))
	return td
}

func GenerateTraceDataOneEmptyInstrumentationLibrary() pdata.Traces {
	td := GenerateTraceDataNoLibraries()
	rs0 := td.ResourceSpans().At(0)
	rs0.InstrumentationLibrarySpans().Resize(1)
	return td
}

func GenerateTraceDataNoLibraries() pdata.Traces {
	td := GenerateTraceDataOneEmptyResourceSpans()
	rs0 := td.ResourceSpans().At(0)
	initResource1(rs0.Resource())
	return td
}

func GenerateTraceDataOneEmptyResourceSpans() pdata.Traces {
	td := GenerateTraceDataEmpty()
	td.ResourceSpans().Resize(1)
	return td
}

func GenerateTraceDataEmpty() pdata.Traces {
	td := pdata.NewTraces()
	return td
}

func initResource1(r pdata.Resource) {
	initResourceAttributes1(r.Attributes())
}

func initResourceAttributes1(dest pdata.AttributeMap) {
	dest.InitFromMap(resourceAttributes1)
}

func fillSpanOne(span pdata.Span) {
	span.SetName("operationA")
	span.SetStartTime(TestSpanStartTimestamp)
	span.SetEndTime(TestSpanEndTimestamp)
	span.SetDroppedAttributesCount(1)
	span.SetTraceID(pdata.NewTraceID([16]byte{0, 1, 2}))
	span.SetSpanID(pdata.NewSpanID([8]byte{0, 1}))
	evs := span.Events()
	evs.Resize(2)
	ev0 := evs.At(0)
	ev0.SetTimestamp(TestSpanEventTimestamp)
	ev0.SetName("event-with-attr")
	initSpanEventAttributes(ev0.Attributes())
	ev0.SetDroppedAttributesCount(2)
	ev1 := evs.At(1)
	ev1.SetTimestamp(TestSpanEventTimestamp)
	ev1.SetName("event")
	ev1.SetDroppedAttributesCount(2)
	span.SetDroppedEventsCount(1)
	status := span.Status()
	status.SetCode(pdata.StatusCodeError)
	status.SetMessage("status-cancelled")
}

func initSpanEventAttributes(dest pdata.AttributeMap) {
	dest.InitFromMap(spanEventAttributes)
}

func jaegerModelToThrift(batch *model.Batch) *jaegerthrift.Batch {
	return &jaegerthrift.Batch{
		Process: jaegerProcessModelToThrift(batch.Process),
		Spans:   jaegerconvert.FromDomain(batch.Spans),
	}
}

func jaegerProcessModelToThrift(process *model.Process) *jaegerthrift.Process {
	if process == nil {
		return nil
	}
	return &jaegerthrift.Process{
		ServiceName: process.ServiceName,
	}
}

func sendToJaegerHTTPThrift(endpoint string, headers map[string]string, batch *jaegerthrift.Batch) error {
	buf, err := thrift.NewTSerializer().Write(context.Background(), batch)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-thrift")
	for k, v := range headers {
		req.Header.Add(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("failed to upload traces; HTTP status code: %d", resp.StatusCode)
	}
	return nil
}
