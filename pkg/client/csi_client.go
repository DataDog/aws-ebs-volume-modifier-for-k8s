package client

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/volume-modifier-for-k8s/pkg/csi-lib-utils/connection"
	"github.com/kubernetes-csi/csi-lib-utils/metrics"
	"github.com/kubernetes-csi/csi-lib-utils/rpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	modifyrpc "github.com/awslabs/volume-modifier-for-k8s/pkg/rpc"
)

type Client interface {
	GetDriverName(context.Context) (string, error)

	SupportsVolumeModification(context.Context) error

	Modify(ctx context.Context, volumeID string, params, reqContext map[string]string) error

	CloseConnection()
}

func New(addr string, timeout time.Duration, metricsmanager metrics.CSIMetricsManager) (Client, error) {
	// Create an OTLP exporter to the OpenTelemetry Collector.
	ctx := context.Background()
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create the OTLP exporter: %w", err)
	}

	resources, err := resource.New(ctx,
		resource.WithFromEnv(), // pull attributes from OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME environment variables
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithHost(),
	)
	if err != nil {
		klog.ErrorS(err, "Failed to create the OTLP resource, spans will lack some metadata")
	}

	// Create a trace provider with the exporter and a sampling strategy.
	traceProvider := trace.NewTracerProvider(trace.WithBatcher(exporter), trace.WithResource(resources), trace.WithSampler(trace.AlwaysSample()))

	// Register the trace provider and propagator as global.
	otel.SetTracerProvider(traceProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Connect to the CSI driver.
	conn, err := connection.ConnectWithOtelGrpcInterceptor(addr, metricsmanager, connection.OnConnectionLoss(connection.ExitOnConnectionLoss()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to CSI driver: %w", err)
	}

	err = rpc.ProbeForever(conn, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed probing CSI driver: %w", err)
	}

	return &client{
		conn: conn,
	}, nil
}

type client struct {
	conn *grpc.ClientConn
}

func (c *client) GetDriverName(ctx context.Context) (string, error) {
	return rpc.GetDriverName(ctx, c.conn)
}

func (c *client) SupportsVolumeModification(ctx context.Context) error {
	cc := modifyrpc.NewModifyClient(c.conn)
	req := &modifyrpc.GetCSIDriverModificationCapabilityRequest{}
	_, err := cc.GetCSIDriverModificationCapability(ctx, req)
	return err
}

func (c *client) Modify(ctx context.Context, volumeID string, params, reqContext map[string]string) error {
	cc := modifyrpc.NewModifyClient(c.conn)
	req := &modifyrpc.ModifyVolumePropertiesRequest{
		Name:       volumeID,
		Parameters: params,
		Context:    reqContext,
	}
	_, err := cc.ModifyVolumeProperties(ctx, req)
	if err == nil {
		klog.V(4).InfoS("Volume modification completed", "volumeID", volumeID)
	}
	return err
}

func (c *client) CloseConnection() {
	c.conn.Close()
}
