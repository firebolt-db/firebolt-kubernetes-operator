package infra

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAwaitBoundPortReturnsPort(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 42915
	port, err := awaitBoundPort(context.Background(), ch, make(chan struct{}), time.Second, []string{"port-forward"})
	if err != nil || port != 42915 {
		t.Fatalf("got (%d, %v), want (42915, nil)", port, err)
	}
}

func TestAwaitBoundPortHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	// A long timeout proves ctx — not the timeout — is what unblocks us.
	_, err := awaitBoundPort(ctx, make(chan int), make(chan struct{}), time.Minute, []string{"port-forward", "-n", "ns"})
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("want cancellation error, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got %v", err)
	}
}

func TestAwaitBoundPortTimesOut(t *testing.T) {
	_, err := awaitBoundPort(context.Background(), make(chan int), make(chan struct{}), time.Millisecond, []string{"port-forward"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want bind-timeout error, got %v", err)
	}
}

func TestAwaitBoundPortFailsFastOnEarlyExit(t *testing.T) {
	stdoutClosed := make(chan struct{})
	close(stdoutClosed) // kubectl exited before printing a Forwarding line
	// A long bind timeout proves the early-exit signal — not the timeout —
	// unblocks us.
	_, err := awaitBoundPort(context.Background(), make(chan int), stdoutClosed, time.Minute, []string{"port-forward"})
	if !errors.Is(err, errEarlyExit) {
		t.Fatalf("want errEarlyExit, got %v", err)
	}
}

func TestAwaitBoundPortPrefersParsedPortOverEarlyExit(t *testing.T) {
	// kubectl printed a Forwarding line (port queued) and then exited
	// (stdout closed). Both cases are ready, so the top-level select may pick
	// either — the parsed port must win, not a false errEarlyExit.
	portCh := make(chan int, 1)
	portCh <- 51000
	stdoutClosed := make(chan struct{})
	close(stdoutClosed)
	port, err := awaitBoundPort(context.Background(), portCh, stdoutClosed, time.Minute, []string{"port-forward"})
	if err != nil || port != 51000 {
		t.Fatalf("got (%d, %v), want (51000, nil) — a parsed port must win over early-exit", port, err)
	}
}

func TestParsePort(t *testing.T) {
	cases := []struct {
		line     string
		wantPort int
		wantOK   bool
	}{
		{"Forwarding from 127.0.0.1:42915 -> 80", 42915, true},
		{"Forwarding from [::1]:42915 -> 80", 0, false},
		{"Handling connection for 42915", 0, false},
	}
	for _, tc := range cases {
		port, ok := parsePort(tc.line)
		if ok != tc.wantOK || port != tc.wantPort {
			t.Errorf("parsePort(%q) = (%d, %v), want (%d, %v)", tc.line, port, ok, tc.wantPort, tc.wantOK)
		}
	}
}
