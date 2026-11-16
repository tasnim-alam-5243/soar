// Package runner is the Go client of the sandboxed Python runner (spec section 10).
// It implements actions.Executor for python-class actions and runs script filters.
package runner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/runner/runnerpb"
)

// Client talks to one runner address (a Kubernetes service or Compose service name).
type Client struct {
	conn *grpc.ClientConn
	rpc  runnerpb.RunnerClient
}

// Dial connects to the runner: over TLS when tc is set (mTLS when tc holds a client
// certificate), otherwise in plain text (the configuration allows that only on a
// private network or to this host).
func Dial(addr string, tc *tls.Config) (*Client, error) {
	creds := insecure.NewCredentials()
	if tc != nil {
		creds = credentials.NewTLS(tc.Clone())
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20), grpc.MaxCallSendMsgSize(8<<20)))
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	return &Client{conn: conn, rpc: runnerpb.NewRunnerClient(conn)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Ping checks that the runner answers (for /readyz).
func (c *Client) Ping(ctx context.Context) error {
	h, err := c.rpc.Health(ctx, &runnerpb.HealthRequest{})
	if err != nil {
		return err
	}
	if !h.Ready {
		return errors.New("runner is not ready")
	}
	return nil
}

// Actions lists the actions the runner can execute.
func (c *Client) Actions(ctx context.Context) ([]string, error) {
	h, err := c.rpc.Health(ctx, &runnerpb.HealthRequest{})
	if err != nil {
		return nil, err
	}
	return h.Actions, nil
}

// Execute implements actions.Executor.
func (c *Client) Execute(ctx context.Context, m actions.Manifest, in actions.Input) (any, error) {
	params, err := json.Marshal(in.Params)
	if err != nil {
		return nil, actions.Errf("invalid_params", false, "params are not JSON: %v", err)
	}
	req := &runnerpb.ExecuteRequest{
		ActionId: m.ID, IdempotencyKey: in.IdempotencyKey, ParamsJson: params, Traceparent: in.Traceparent,
		Connector: m.Connector, Entrypoint: m.Entrypoint, DryRun: in.DryRun,
		ExecutionId: in.ExecutionID, NodeId: in.NodeID, Attempt: uint32(in.Attempt),
		Limits: &runnerpb.Limits{Cpu: m.Limits.CPU, MemoryMb: uint32(m.Limits.MemoryMB), Egress: m.Limits.Egress},
	}
	if dl, ok := ctx.Deadline(); ok {
		secs := time.Until(dl).Seconds() - 1
		if secs < 1 {
			secs = 1
		}
		req.Limits.TimeoutS = uint32(secs)
	} else if m.Limits.TimeoutS > 0 {
		req.Limits.TimeoutS = uint32(m.Limits.TimeoutS)
	}
	if in.Connection != nil {
		req.Secrets = in.Connection.Secrets
		cfg := in.Connection.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		if req.ConnectionJson, err = json.Marshal(cfg); err != nil {
			return nil, actions.Errf("invalid_connection", false, "connection config is not JSON: %v", err)
		}
	}
	stream, err := c.rpc.Execute(ctx, req)
	if err != nil {
		return nil, rpcError(err)
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, actions.Errf("runner_protocol", true, "the runner closed the stream without a result")
		}
		if err != nil {
			return nil, rpcError(err)
		}
		switch e := ev.Event.(type) {
		case *runnerpb.ExecuteEvent_Log:
			if in.Log != nil {
				in.Log(e.Log)
			}
		case *runnerpb.ExecuteEvent_Result:
			var out any
			if len(e.Result.OutputJson) > 0 {
				if err := json.Unmarshal(e.Result.OutputJson, &out); err != nil {
					return nil, actions.Errf("invalid_output", false, "runner output is not JSON: %v", err)
				}
			}
			return out, nil
		case *runnerpb.ExecuteEvent_Failure:
			return nil, &actions.Error{Code: e.Failure.Code, Retryable: e.Failure.Retryable, Message: e.Failure.Message}
		}
	}
}

// Filter runs a script filter over a batch of events and returns one decision per event.
func (c *Client) Filter(ctx context.Context, script string, events []map[string]any, timeout time.Duration) ([]string, error) {
	data, err := json.Marshal(events)
	if err != nil {
		return nil, err
	}
	resp, err := c.rpc.Filter(ctx, &runnerpb.FilterRequest{
		Script: script, EventsJson: data,
		Limits: &runnerpb.Limits{TimeoutS: uint32(timeout.Seconds()), MemoryMb: 256},
	})
	if err != nil {
		return nil, rpcError(err)
	}
	if resp.Failure != nil {
		return nil, &actions.Error{Code: resp.Failure.Code, Message: resp.Failure.Message, Retryable: resp.Failure.Retryable}
	}
	if len(resp.Decisions) != len(events) {
		return nil, fmt.Errorf("runner returned %d decisions for %d events", len(resp.Decisions), len(events))
	}
	return resp.Decisions, nil
}

func rpcError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return actions.Errf("timeout", true, "the runner did not answer in time")
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.ResourceExhausted, codes.Aborted:
		return actions.Errf("runner_unavailable", true, "%v", err)
	case codes.DeadlineExceeded:
		return actions.Errf("timeout", true, "%v", err)
	case codes.Canceled:
		return actions.Errf("cancelled", true, "%v", err)
	}
	return actions.Errf("runner_error", false, "%v", err)
}
