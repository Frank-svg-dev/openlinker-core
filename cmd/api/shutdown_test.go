package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestWaitForCoreStopFatalServeErrorWinsConcurrentSignal(t *testing.T) {
	fatalErr := errors.New("listener failed")
	for iteration := 0; iteration < 1000; iteration++ {
		signals := make(chan os.Signal, 1)
		serveErrors := make(chan error, 1)
		signals <- syscall.SIGTERM
		serveErrors <- fatalErr
		if err := waitForCoreStop(signals, serveErrors); !errors.Is(err, fatalErr) {
			t.Fatalf("iteration %d stop error = %v, want fatal serve error", iteration, err)
		}
	}

	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	if err := waitForCoreStop(signals, make(chan error)); err != nil {
		t.Fatalf("signal-only stop error = %v", err)
	}

	serveErrors := make(chan error, 1)
	serveErrors <- fatalErr
	if err := waitForCoreStop(make(chan os.Signal), serveErrors); !errors.Is(err, fatalErr) {
		t.Fatalf("fatal-only stop error = %v", err)
	}
}

func TestRunCoreShutdownUsesStrictOrderAndIndependentContexts(t *testing.T) {
	var order []string
	phase := func(name string) coreShutdownFunc {
		return func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatalf("%s received an expired context", name)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatalf("%s received a context without a deadline", name)
			}
			order = append(order, name)
			return nil
		}
	}
	result, err := runCoreShutdown(coreShutdownPlan{
		PhaseTimeout:              time.Second,
		RuntimeAttachOnly:         true,
		ShutdownRuntimeController: phase("controller"),
		ShutdownHTTP:              phase("http"),
		ShutdownRuntimeMTLS:       phase("mtls"),
		DetachSessions: func(ctx context.Context) (int64, error) {
			if ctx.Err() != nil {
				t.Fatal("detach received an expired context")
			}
			order = append(order, "detach")
			return 9, nil
		},
		ShutdownA2AGRPC:     phase("a2a"),
		CloseRuntimeCluster: phase("cluster"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.DetachCompleted || result.DetachedSessions != 9 {
		t.Fatalf("shutdown result = %#v", result)
	}
	want := []string{"controller", "http", "mtls", "detach", "a2a", "cluster"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("shutdown order = %#v, want %#v", order, want)
	}
}

func TestRunCoreShutdownTimeoutSkipsDetachButContinuesWithFreshContexts(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	detachCalled := false
	result, err := runCoreShutdown(coreShutdownPlan{
		PhaseTimeout:      20 * time.Millisecond,
		RuntimeAttachOnly: true,
		ShutdownRuntimeController: func(ctx context.Context) error {
			record("controller")
			<-ctx.Done()
			return ctx.Err()
		},
		ShutdownHTTP: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatal("HTTP inherited the expired controller context")
			}
			record("http")
			return nil
		},
		ShutdownRuntimeMTLS: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatal("mTLS inherited an expired shutdown context")
			}
			record("mtls")
			return nil
		},
		DetachSessions: func(context.Context) (int64, error) {
			detachCalled = true
			return 0, nil
		},
		ShutdownA2AGRPC: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatal("A2A inherited an expired shutdown context")
			}
			record("a2a")
			return nil
		},
		CloseRuntimeCluster: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatal("cluster inherited an expired shutdown context")
			}
			record("cluster")
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime websocket shutdown: context deadline exceeded") {
		t.Fatalf("shutdown error = %v", err)
	}
	if detachCalled || result.DetachCompleted {
		t.Fatalf("detach ran after an incomplete ingress shutdown: result=%#v called=%t", result, detachCalled)
	}
	want := []string{"controller", "http", "mtls", "a2a", "cluster"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("shutdown order = %#v, want %#v", order, want)
	}
}

func TestRunCoreShutdownReturnsDetachAndClusterFailuresAfterOrderedCleanup(t *testing.T) {
	detachErr := errors.New("detach blocked")
	clusterErr := errors.New("cluster close blocked")
	var order []string
	phase := func(name string, err error) coreShutdownFunc {
		return func(context.Context) error {
			order = append(order, name)
			return err
		}
	}
	result, err := runCoreShutdown(coreShutdownPlan{
		RuntimeAttachOnly:         true,
		ShutdownRuntimeController: phase("controller", nil),
		ShutdownHTTP:              phase("http", nil),
		DetachSessions: func(context.Context) (int64, error) {
			order = append(order, "detach")
			return 3, detachErr
		},
		ShutdownA2AGRPC:     phase("a2a", nil),
		CloseRuntimeCluster: phase("cluster", clusterErr),
	})
	if !errors.Is(err, detachErr) || !errors.Is(err, clusterErr) {
		t.Fatalf("shutdown error = %v", err)
	}
	if result.DetachCompleted || result.DetachedSessions != 0 {
		t.Fatalf("failed detach was reported as complete: %#v", result)
	}
	want := []string{"controller", "http", "detach", "a2a", "cluster"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("shutdown order = %#v, want %#v", order, want)
	}
}
