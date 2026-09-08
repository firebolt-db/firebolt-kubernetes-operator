// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package wakeagent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"

	corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typepb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Process keeps each grant alive across router retries. Envoy must enable
// ext_proc_graceful_grpc_close and response-header processing: clean EOF then
// establishes that its filter can no longer cause upstream dispatch. A reset or
// transport failure cannot establish that fact and leaves an unknown permit.
func (a *Agent) Process(stream extpb.ExternalProcessor_ProcessServer) error {
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	headers := request.GetRequestHeaders()
	if headers == nil {
		return status.Error(codes.InvalidArgument, "expected request headers")
	}
	engine := ""
	for _, header := range headers.GetHeaders().GetHeaders() {
		if strings.EqualFold(header.GetKey(), "x-firebolt-engine") {
			if engine != "" {
				return status.Error(codes.InvalidArgument, "duplicate engine header")
			}
			engine = string(header.GetRawValue())
			if engine == "" {
				engine = header.GetValue()
			}
		}
	}
	if !isValidEngineName(engine) {
		return sendRejection(stream, typepb.StatusCode_BadRequest, "invalid engine name")
	}

	// Read concurrently while admission waits. A clean downstream cancellation
	// half-closes the processor stream without canceling its gRPC context, so an
	// admission wait must observe Recv as well as Context.Done.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	type received struct {
		request *extpb.ProcessingRequest
		err     error
	}
	incoming := make(chan received, 1)
	var cleanEOF atomic.Bool
	go func() {
		for {
			request, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				cleanEOF.Store(true)
			}
			if err != nil {
				cancel()
			}
			select {
			case incoming <- received{request, err}:
			case <-ctx.Done():
				// Deliver the terminal result even after canceling admission.
				if err != nil {
					select {
					case incoming <- received{request, err}:
					default:
					}
				}
				return
			}
			if err != nil {
				return
			}
		}
	}()
	route, err := a.acquireRoute(ctx, engine)
	if err != nil {
		if ctx.Err() != nil {
			return status.Error(codes.Canceled, "request canceled before admission")
		}
		return sendRejection(stream, typepb.StatusCode_ServiceUnavailable, "engine route unavailable")
	}
	clean := false
	defer func() { a.finishPermit(route.Key(), clean || cleanEOF.Load()) }()
	mutation := &extpb.HeaderMutation{SetHeaders: []*corepb.HeaderValueOption{{
		Header:       &corepb.HeaderValue{Key: ":authority", RawValue: []byte(route.Authority)},
		AppendAction: corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}}}
	if err := stream.Send(&extpb.ProcessingResponse{Response: &extpb.ProcessingResponse_RequestHeaders{
		RequestHeaders: &extpb.HeadersResponse{Response: &extpb.CommonResponse{HeaderMutation: mutation, ClearRouteCache: true}},
	}}); err != nil {
		return err
	}
	for {
		var next received
		select {
		case next = <-incoming:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		if next.err != nil {
			clean = errors.Is(next.err, io.EOF)
			if clean {
				return nil
			}
			return next.err
		}
		if next.request.GetResponseHeaders() == nil {
			return status.Error(codes.InvalidArgument, "unexpected processing message")
		}
		if err := stream.Send(&extpb.ProcessingResponse{Response: &extpb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extpb.HeadersResponse{Response: &extpb.CommonResponse{}},
		}}); err != nil {
			return err
		}
	}
}

func sendRejection(stream extpb.ExternalProcessor_ProcessServer, code typepb.StatusCode, body string) error {
	return stream.Send(&extpb.ProcessingResponse{Response: &extpb.ProcessingResponse_ImmediateResponse{
		ImmediateResponse: &extpb.ImmediateResponse{Status: &typepb.HttpStatus{Code: code}, Body: []byte(body)},
	}})
}
