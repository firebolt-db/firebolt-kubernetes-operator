//go:build e2e
// +build e2e

/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"errors"
	"testing"
)

func TestRetryGatewayQueryOnDNSFailure(t *testing.T) {
	dnsTimeout := errors.New("curl failed: curl: (28) Resolving timed out after 2002 milliseconds")
	dnsMissing := errors.New("curl failed: curl: (6) Could not resolve host: gateway.test.svc.cluster.local")
	connectionReset := errors.New("curl failed: recv failure: Connection reset by peer")
	httpFailure := errors.New("curl failed: the requested URL returned error: 503")
	genericTimeout := errors.New("curl failed: operation timed out while awaiting response")

	tests := []struct {
		name      string
		results   []error
		wantCalls int
		wantErr   error
	}{
		{
			name:      "success without retry",
			results:   []error{nil},
			wantCalls: 1,
		},
		{
			name:      "resolving timeout is retried",
			results:   []error{dnsTimeout, nil},
			wantCalls: 2,
		},
		{
			name:      "missing DNS record is retried",
			results:   []error{dnsMissing, dnsMissing, nil},
			wantCalls: 3,
		},
		{
			name:      "DNS retries are bounded",
			results:   []error{dnsTimeout, dnsMissing, dnsTimeout},
			wantCalls: 3,
			wantErr:   dnsTimeout,
		},
		{
			name:      "connection failure is not retried",
			results:   []error{connectionReset, nil},
			wantCalls: 1,
			wantErr:   connectionReset,
		},
		{
			name:      "HTTP failure is not retried",
			results:   []error{httpFailure, nil},
			wantCalls: 1,
			wantErr:   httpFailure,
		},
		{
			name:      "generic request timeout is not retried",
			results:   []error{genericTimeout, nil},
			wantCalls: 1,
			wantErr:   genericTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			output, err := retryGatewayQueryOnDNSFailure(context.Background(), func(context.Context) (string, error) {
				result := tt.results[calls]
				calls++
				if result != nil {
					return "", result
				}
				return "ok", nil
			})

			if calls != tt.wantCalls {
				t.Fatalf("query calls = %d, want %d", calls, tt.wantCalls)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && output != "ok" {
				t.Fatalf("output = %q, want ok", output)
			}
		})
	}
}

func TestGatewayDNSResolutionFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", want: false},
		{name: "could not resolve", err: errors.New("curl: (6) Could not resolve host: gateway"), want: true},
		{name: "resolving timeout", err: errors.New("curl: (28) Resolving timed out after 2002 milliseconds"), want: true},
		{name: "connection refused", err: errors.New("curl: (7) Failed to connect: Connection refused"), want: false},
		{name: "connection reset", err: errors.New("curl: (56) Recv failure: Connection reset by peer"), want: false},
		{name: "HTTP 503", err: errors.New("curl: (22) The requested URL returned error: 503"), want: false},
		{name: "generic timeout", err: errors.New("curl: (28) Operation timed out after 33001 milliseconds"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGatewayDNSResolutionFailure(tt.err); got != tt.want {
				t.Fatalf("isGatewayDNSResolutionFailure(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}
