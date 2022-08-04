//go:generate ../../../tools/readme_config_includer/generator
package opentelemetry

import (
	"context"
	_ "embed"
	"fmt"
	"runtime"
	"time"

	ntls "crypto/tls"

	"github.com/influxdata/influxdb-observability/common"
	"github.com/influxdata/influxdb-observability/influx2otel"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	// Blank import to allow gzip encoding
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/common/tls"
	"github.com/influxdata/telegraf/plugins/outputs"
)

// DO NOT REMOVE THE NEXT TWO LINES! This is required to embed the sampleConfig data.
//go:embed sample.conf
var sampleConfig string

type OpenTelemetry struct {
	ServiceAddress string `toml:"service_address"`

	tls.ClientConfig
	Timeout     config.Duration   `toml:"timeout"`
	Compression string            `toml:"compression"`
	Headers     map[string]string `toml:"headers"`
	Attributes  map[string]string `toml:"attributes"`
	coralogix *CoralogixConfig `toml:"coralogix"`

	Log telegraf.Logger `toml:"-"`

	metricsConverter     *influx2otel.LineProtocolToOtelMetrics
	grpcClientConn       *grpc.ClientConn
	metricsServiceClient pmetricotlp.Client
	callOptions          []grpc.CallOption
}

const coralogixDialect = "coralogix"

type CoralogixConfig struct {
	AppName    string `toml:"application_name"`
	SubSystem  string `toml:"subsystem_name"`
	PrivateKey string `toml:"private_key"`
}

func (*OpenTelemetry) SampleConfig() string {
	return sampleConfig
}

func (o *OpenTelemetry) Connect() error {
	logger := &otelLogger{o.Log}

	if o.ServiceAddress == "" {
		o.ServiceAddress = defaultServiceAddress
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.Compression == "" {
		o.Compression = defaultCompression
	}
	if o.Dialect == coralogixDialect {
		if o.Headers == nil {
			o.Headers = make(map[string]string)
		}
		o.Headers["ApplicationName"] = o.CoralogixConfig.AppName
		o.Headers["ApiName"] = o.CoralogixConfig.SubSystem
		o.Headers["Authorization"] = "Bearer " + o.CoralogixConfig.PrivateKey
	}

	metricsConverter, err := influx2otel.NewLineProtocolToOtelMetrics(logger)
	if err != nil {
		return err
	}

	var grpcTLSDialOption grpc.DialOption
	if tlsConfig, err := o.ClientConfig.TLSConfig(); err != nil {
		return err
	} else if tlsConfig != nil {
		grpcTLSDialOption = grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))
	} else if o.Dialect == coralogixDialect {
		// For coralogix, we default to GRPC connection with TLS using native Go TLS package
		grpcTLSDialOption = grpc.WithTransportCredentials(credentials.NewTLS(&ntls.Config{}))
	} else {
		grpcTLSDialOption = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	userAgent := fmt.Sprintf("telegraf (%s/%s)", runtime.GOOS, runtime.GOARCH)

	grpcClientConn, err := grpc.Dial(o.ServiceAddress, grpcTLSDialOption, grpc.WithUserAgent(userAgent))
	if err != nil {
		return err
	}

	metricsServiceClient := pmetricotlp.NewClient(grpcClientConn)

	o.metricsConverter = metricsConverter
	o.grpcClientConn = grpcClientConn
	o.metricsServiceClient = metricsServiceClient

	if o.Compression != "" && o.Compression != "none" {
		o.callOptions = append(o.callOptions, grpc.UseCompressor(o.Compression))
	}

	return nil
}

func (o *OpenTelemetry) Close() error {
	if o.grpcClientConn != nil {
		err := o.grpcClientConn.Close()
		o.grpcClientConn = nil
		return err
	}
	return nil
}

func (o *OpenTelemetry) Write(metrics []telegraf.Metric) error {
	batch := o.metricsConverter.NewBatch()
	for _, metric := range metrics {
		var vType common.InfluxMetricValueType
		switch metric.Type() {
		case telegraf.Gauge:
			vType = common.InfluxMetricValueTypeGauge
		case telegraf.Untyped:
			vType = common.InfluxMetricValueTypeUntyped
		case telegraf.Counter:
			vType = common.InfluxMetricValueTypeSum
		case telegraf.Histogram:
			vType = common.InfluxMetricValueTypeHistogram
		case telegraf.Summary:
			vType = common.InfluxMetricValueTypeSummary
		default:
			o.Log.Warnf("unrecognized metric type %Q", metric.Type())
			continue
		}
		err := batch.AddPoint(metric.Name(), metric.Tags(), metric.Fields(), metric.Time(), vType)
		if err != nil {
			o.Log.Warnf("failed to add point: %s", err)
			continue
		}
	}

	md := pmetricotlp.NewRequestFromMetrics(batch.GetMetrics())
	if md.Metrics().ResourceMetrics().Len() == 0 {
		return nil
	}

	if len(o.Attributes) > 0 {
		for i := 0; i < md.Metrics().ResourceMetrics().Len(); i++ {
			for k, v := range o.Attributes {
				md.Metrics().ResourceMetrics().At(i).Resource().Attributes().UpsertString(k, v)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(o.Timeout))

	if len(o.Headers) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(o.Headers))
	}
	defer cancel()
	_, err := o.metricsServiceClient.Export(ctx, md, o.callOptions...)
	return err
}

const (
	defaultServiceAddress = "localhost:4317"
	defaultTimeout        = config.Duration(5 * time.Second)
	defaultCompression    = "gzip"
)

func init() {
	outputs.Add("opentelemetry", func() telegraf.Output {
		return &OpenTelemetry{
			ServiceAddress: defaultServiceAddress,
			Timeout:        defaultTimeout,
			Compression:    defaultCompression,
		}
	})
}
