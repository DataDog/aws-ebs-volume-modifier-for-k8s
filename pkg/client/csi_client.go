package client

import (
	"context"
	"fmt"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/connection"
	"github.com/kubernetes-csi/csi-lib-utils/metrics"
	"github.com/kubernetes-csi/csi-lib-utils/rpc"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

type Client interface {
	GetDriverName(context.Context) (string, error)

	SupportsVolumeModification(context.Context) error

	Modify(ctx context.Context, volumeID string, params, reqContext map[string]string) error

	CloseConnection()
}

func New(addr string, timeout time.Duration, metricsmanager metrics.CSIMetricsManager) (Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := connection.Connect(ctx, addr, metricsmanager, connection.OnConnectionLoss(connection.ExitOnConnectionLoss()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to CSI driver: %w", err)
	}

	err = rpc.ProbeForever(ctx, conn, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed probing CSI driver: %w", err)
	}

	csiClient := csi.NewControllerClient(conn)

	return &client{
		conn:      conn,
		csiClient: csiClient,
	}, nil
}

type client struct {
	conn      *grpc.ClientConn
	csiClient csi.ControllerClient
}

func (c *client) GetDriverName(ctx context.Context) (string, error) {
	return rpc.GetDriverName(ctx, c.conn)
}

func (c *client) SupportsVolumeModification(ctx context.Context) error {
	controllerCapabilities, err := rpc.GetControllerCapabilities(ctx, c.conn)
	if err != nil {
		return err
	}
	if !controllerCapabilities[csi.ControllerServiceCapability_RPC_MODIFY_VOLUME] {
		return fmt.Errorf("CSI driver does not support volume modification")
	}
	return nil
}

func (c *client) Modify(ctx context.Context, volumeID string, params, reqContext map[string]string) error {
	req := &csi.ControllerModifyVolumeRequest{
		VolumeId:          volumeID,
		MutableParameters: params,
	}
	_, err := c.csiClient.ControllerModifyVolume(ctx, req)
	if err == nil {
		klog.V(4).InfoS("Volume modification completed", "volumeID", volumeID)
	}
	return err
}

func (c *client) CloseConnection() {
	c.conn.Close()
}
