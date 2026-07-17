package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestParseRuntimeMTLSEdgeConfigDefaultsAndOverrides(t *testing.T) {
	cfg, err := parseRuntimeMTLSEdgeConfig(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != defaultRuntimeMTLSEdgeListenAddress || cfg.UpstreamAddress != defaultRuntimeMTLSEdgeUpstreamAddress {
		t.Fatalf("unexpected default addresses: %#v", cfg)
	}
	if cfg.DialTimeout != defaultRuntimeMTLSEdgeDialTimeout || cfg.IdleTimeout != defaultRuntimeMTLSEdgeIdleTimeout || cfg.ShutdownTimeout != defaultRuntimeMTLSEdgeShutdownTimeout {
		t.Fatalf("unexpected default timeouts: %#v", cfg)
	}
	if cfg.MaxConnections != defaultRuntimeMTLSEdgeMaxConnections {
		t.Fatalf("default max connections = %d", cfg.MaxConnections)
	}

	environment := map[string]string{
		"OPENLINKER_RUNTIME_EDGE_LISTEN":           "127.0.0.1:9443",
		"OPENLINKER_RUNTIME_EDGE_UPSTREAM":         "runtime-core:9443",
		"OPENLINKER_RUNTIME_EDGE_DIAL_TIMEOUT":     "7s",
		"OPENLINKER_RUNTIME_EDGE_IDLE_TIMEOUT":     "3m",
		"OPENLINKER_RUNTIME_EDGE_SHUTDOWN_TIMEOUT": "11s",
		"OPENLINKER_RUNTIME_EDGE_MAX_CONNECTIONS":  "123",
	}
	environmentCfg, err := parseRuntimeMTLSEdgeConfig(nil, func(key string) string { return environment[key] })
	if err != nil {
		t.Fatal(err)
	}
	if environmentCfg.MaxConnections != 123 {
		t.Fatalf("environment max connections = %d", environmentCfg.MaxConnections)
	}

	cfg, err = parseRuntimeMTLSEdgeConfig([]string{
		"--listen", "127.0.0.1:10443",
		"--upstream", "core-api:11443",
		"--dial-timeout", "750ms",
		"--idle-timeout", "45s",
		"--shutdown-timeout", "4s",
		"--max-connections", "17",
	}, func(key string) string { return environment[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "127.0.0.1:10443" || cfg.UpstreamAddress != "core-api:11443" {
		t.Fatalf("flags did not override environment: %#v", cfg)
	}
	if cfg.DialTimeout != 750*time.Millisecond || cfg.IdleTimeout != 45*time.Second || cfg.ShutdownTimeout != 4*time.Second {
		t.Fatalf("unexpected overridden timeouts: %#v", cfg)
	}
	if cfg.MaxConnections != 17 {
		t.Fatalf("flag max connections = %d", cfg.MaxConnections)
	}
}

func TestParseRuntimeMTLSEdgeConfigFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"--unknown"}},
		{name: "positional data", args: []string{"secret-value"}},
		{name: "listen URL", args: []string{"--listen", "https://127.0.0.1:8443"}},
		{name: "listen missing port", args: []string{"--listen", "127.0.0.1"}},
		{name: "listen zero port", args: []string{"--listen", "127.0.0.1:0"}},
		{name: "upstream empty host", args: []string{"--upstream", ":8443"}},
		{name: "upstream user info", args: []string{"--upstream", "user@core-api:8443"}},
		{name: "upstream invalid port", args: []string{"--upstream", "core-api:65536"}},
		{name: "exact self loop", args: []string{"--listen", "127.0.0.1:8443", "--upstream", "127.0.0.1:8443"}},
		{name: "wildcard IPv4 self loop", args: []string{"--listen", ":8443", "--upstream", "127.0.0.1:8443"}},
		{name: "wildcard IPv6 self loop", args: []string{"--listen", "[::]:8443", "--upstream", "[::1]:8443"}},
		{name: "localhost self loop", args: []string{"--listen", "localhost:8443", "--upstream", "127.0.0.1:8443"}},
		{name: "healthcheck invalid listen", args: []string{"--healthcheck", "--listen", "not-an-address"}},
		{name: "healthcheck invalid upstream", args: []string{"--healthcheck", "--upstream", ":8443"}},
		{name: "zero dial timeout", args: []string{"--dial-timeout", "0s"}},
		{name: "excessive dial timeout", args: []string{"--dial-timeout", "61s"}},
		{name: "invalid idle timeout", args: []string{"--idle-timeout", "forever"}},
		{name: "excessive shutdown timeout", args: []string{"--shutdown-timeout", "2m"}},
		{name: "invalid max connections", args: []string{"--max-connections", "many"}},
		{name: "zero max connections", args: []string{"--max-connections", "0"}},
		{name: "excessive max connections", args: []string{"--max-connections", "65536"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseRuntimeMTLSEdgeConfig(test.args, nil); !errors.Is(err, errRuntimeMTLSEdgeInvalidFlags) {
				t.Fatalf("error = %v, want invalid flags", err)
			}
		})
	}
}

func TestRunRuntimeMTLSEdgeHelpAndErrorsDoNotEchoInputs(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := runRuntimeMTLSEdgeWithContext(context.Background(), []string{"--help"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "usage: api runtime-mtls-edge") || stderr.Len() != 0 {
		t.Fatalf("unexpected help output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	const secret = "credential-that-must-not-be-logged"
	stdout.Reset()
	stderr.Reset()
	code := runRuntimeMTLSEdgeWithDependencies(
		context.Background(),
		[]string{"--listen", "127.0.0.1:9443"},
		nil,
		&stdout,
		&stderr,
		runtimeMTLSEdgeDependencies{
			Listen: func(string, string) (net.Listener, error) { return nil, errors.New(secret) },
		},
	)
	if code != 1 || !strings.Contains(stderr.String(), "listen failed") || strings.Contains(stderr.String(), secret) {
		t.Fatalf("unsafe listen failure output: code=%d stderr=%q", code, stderr.String())
	}

	stderr.Reset()
	code = runRuntimeMTLSEdgeWithContext(context.Background(), []string{secret}, nil, io.Discard, &stderr)
	if code != 2 || strings.Contains(stderr.String(), secret) {
		t.Fatalf("unsafe argument failure output: code=%d stderr=%q", code, stderr.String())
	}
}

func TestRuntimeMTLSEdgeHealthcheckUsesListenEnvironmentAndConfiguredUpstream(t *testing.T) {
	const upstreamAddress = "runtime-core:9443"
	var gotNetworks []string
	var gotAddresses []string
	var deadlineRemaining []time.Duration
	peerClosed := make(chan struct{}, 2)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		gotNetworks = append(gotNetworks, network)
		gotAddresses = append(gotAddresses, address)
		if deadline, ok := ctx.Deadline(); ok {
			deadlineRemaining = append(deadlineRemaining, time.Until(deadline))
		}
		local, peer := net.Pipe()
		go func() {
			defer func() { peerClosed <- struct{}{} }()
			_, _ = peer.Read(make([]byte, 1))
			_ = peer.Close()
		}()
		return local, nil
	}
	var stderr bytes.Buffer
	code := runRuntimeMTLSEdgeWithDependencies(
		context.Background(),
		[]string{"--healthcheck", "--upstream", upstreamAddress, "--dial-timeout", "250ms"},
		func(key string) string {
			if key == "OPENLINKER_RUNTIME_EDGE_LISTEN" {
				return "0.0.0.0:9443"
			}
			return ""
		},
		io.Discard,
		&stderr,
		runtimeMTLSEdgeDependencies{
			Listen: func(string, string) (net.Listener, error) {
				t.Fatal("healthcheck must not open a listener")
				return nil, nil
			},
			Dial: dial,
		},
	)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("healthcheck code=%d stderr=%q", code, stderr.String())
	}
	if len(gotNetworks) != 2 || gotNetworks[0] != "tcp" || gotNetworks[1] != "tcp" {
		t.Fatalf("healthcheck networks = %#v", gotNetworks)
	}
	wantAddresses := []string{"127.0.0.1:9443", upstreamAddress}
	if len(gotAddresses) != len(wantAddresses) || gotAddresses[0] != wantAddresses[0] || gotAddresses[1] != wantAddresses[1] {
		t.Fatalf("healthcheck addresses = %#v, want %#v", gotAddresses, wantAddresses)
	}
	if len(deadlineRemaining) != 2 {
		t.Fatalf("healthcheck deadlines = %#v", deadlineRemaining)
	}
	for index, remaining := range deadlineRemaining {
		if remaining <= 0 || remaining > 250*time.Millisecond {
			t.Fatalf("healthcheck deadline %d remaining = %s", index, remaining)
		}
	}
	for range wantAddresses {
		select {
		case <-peerClosed:
		case <-time.After(time.Second):
			t.Fatal("healthcheck connection was not closed")
		}
	}
}

func TestRuntimeMTLSEdgeHealthcheckTargetRespectsConfiguredListenAddress(t *testing.T) {
	for _, test := range []struct {
		listen string
		want   string
	}{
		{listen: ":9443", want: "127.0.0.1:9443"},
		{listen: "0.0.0.0:9555", want: "127.0.0.1:9555"},
		{listen: "[::]:9666", want: "[::1]:9666"},
		{listen: "192.0.2.8:9777", want: "192.0.2.8:9777"},
		{listen: "localhost:9888", want: "localhost:9888"},
	} {
		t.Run(test.listen, func(t *testing.T) {
			got, ok := runtimeMTLSEdgeHealthcheckTarget(test.listen)
			if !ok || got != test.want {
				t.Fatalf("healthcheck target = %q, %t; want %q, true", got, ok, test.want)
			}
		})
	}
	if _, ok := runtimeMTLSEdgeHealthcheckTarget("not-an-address"); ok {
		t.Fatal("invalid listen address produced a healthcheck target")
	}
}

func TestRuntimeMTLSEdgeResolvedSelfCheckRejectsLocalAliasesWithoutRejectingRemoteCore(t *testing.T) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	interfaces := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("10.20.0.2"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		}, nil
	}
	lookup := func(_ context.Context, _, host string) ([]net.IP, error) {
		switch host {
		case "edge-alias":
			return []net.IP{net.ParseIP("10.20.0.2")}, nil
		case "core-api":
			return []net.IP{net.ParseIP("10.20.0.9")}, nil
		case "missing":
			return nil, errors.New("dns unavailable")
		default:
			return nil, fmt.Errorf("unexpected host %q", host)
		}
	}
	base := runtimeMTLSEdgeConfig{DialTimeout: time.Second}

	for _, upstream := range []string{
		net.JoinHostPort("edge-alias", port),
		net.JoinHostPort("10.20.0.2", port),
	} {
		cfg := base
		cfg.UpstreamAddress = upstream
		if err := validateRuntimeMTLSEdgeResolvedUpstream(
			context.Background(), listener, cfg, lookup, interfaces,
		); !errors.Is(err, errRuntimeMTLSEdgeSelfCheck) {
			t.Fatalf("local upstream %q error = %v, want self-check failure", upstream, err)
		}
	}

	cfg := base
	cfg.UpstreamAddress = net.JoinHostPort("core-api", port)
	if err := validateRuntimeMTLSEdgeResolvedUpstream(
		context.Background(), listener, cfg, lookup, interfaces,
	); err != nil {
		t.Fatalf("remote core-api was rejected: %v", err)
	}

	cfg.UpstreamAddress = net.JoinHostPort("missing", port)
	if err := validateRuntimeMTLSEdgeResolvedUpstream(
		context.Background(), listener, cfg, lookup, interfaces,
	); !errors.Is(err, errRuntimeMTLSEdgeSelfCheck) {
		t.Fatalf("unresolved same-port upstream error = %v, want fail closed", err)
	}

	differentPort := "1"
	if port == differentPort {
		differentPort = "2"
	}
	cfg.UpstreamAddress = net.JoinHostPort("missing", differentPort)
	if err := validateRuntimeMTLSEdgeResolvedUpstream(
		context.Background(), listener, cfg, lookup, interfaces,
	); err != nil {
		t.Fatalf("different-port upstream should not require self-loop resolution: %v", err)
	}
}

func TestRuntimeMTLSEdgeHealthcheckRejectsInvalidAddressesBeforeDial(t *testing.T) {
	for _, test := range []struct {
		name          string
		flag          string
		secretAddress string
	}{
		{name: "listen", flag: "--listen", secretAddress: "credential@127.0.0.1:8443"},
		{name: "upstream", flag: "--upstream", secretAddress: "credential@core-api:8443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			dialed := false
			code := runRuntimeMTLSEdgeWithDependencies(
				context.Background(),
				[]string{"--healthcheck", test.flag, test.secretAddress},
				nil,
				io.Discard,
				&stderr,
				runtimeMTLSEdgeDependencies{
					Dial: func(context.Context, string, string) (net.Conn, error) {
						dialed = true
						return nil, nil
					},
				},
			)
			if code != 2 || dialed || !strings.Contains(stderr.String(), "invalid arguments") || strings.Contains(stderr.String(), test.secretAddress) {
				t.Fatalf("invalid healthcheck address was not rejected safely: code=%d dialed=%t stderr=%q", code, dialed, stderr.String())
			}
		})
	}
}

func TestRuntimeMTLSEdgeHealthcheckFailuresAreSafe(t *testing.T) {
	const secret = "dial-error-with-sensitive-data"
	for _, failedAddress := range []string{"127.0.0.1:8443", "core-api:8443"} {
		t.Run(failedAddress, func(t *testing.T) {
			var dialed []string
			var stderr bytes.Buffer
			code := runRuntimeMTLSEdgeWithDependencies(
				context.Background(),
				[]string{"--healthcheck", "--dial-timeout", "20ms"},
				nil,
				io.Discard,
				&stderr,
				runtimeMTLSEdgeDependencies{
					Dial: func(_ context.Context, _, address string) (net.Conn, error) {
						dialed = append(dialed, address)
						if address == failedAddress {
							return nil, errors.New(secret)
						}
						local, peer := net.Pipe()
						_ = peer.Close()
						return local, nil
					},
				},
			)
			if code != 1 || !strings.Contains(stderr.String(), "healthcheck failed") || strings.Contains(stderr.String(), secret) {
				t.Fatalf("unsafe healthcheck failure: code=%d stderr=%q", code, stderr.String())
			}
			wantDialed := []string{"127.0.0.1:8443", "core-api:8443"}
			if len(dialed) != len(wantDialed) || dialed[0] != wantDialed[0] || dialed[1] != wantDialed[1] {
				t.Fatalf("healthcheck dialed %#v, want %#v", dialed, wantDialed)
			}
		})
	}
}

func TestRuntimeMTLSEdgePassesBytesTransparentlyAndPreservesHalfClose(t *testing.T) {
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamListener.Close()
	payload := append([]byte{0x16, 0x03, 0x03, 0x00, 0x11, 0x00, 0xff, 0x00}, bytes.Repeat([]byte("opaque-runtime-mtls\x00\xff"), 8192)...)
	upstreamReceived := make(chan []byte, 1)
	upstreamErr := make(chan error, 1)
	go func() {
		conn, acceptErr := upstreamListener.Accept()
		if acceptErr != nil {
			upstreamErr <- acceptErr
			return
		}
		defer conn.Close()
		received, readErr := io.ReadAll(conn)
		if readErr != nil {
			upstreamErr <- readErr
			return
		}
		upstreamReceived <- received
		if _, writeErr := conn.Write(received); writeErr != nil {
			upstreamErr <- writeErr
			return
		}
		if closeWriter, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		upstreamErr <- nil
	}()

	edgeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- serveRuntimeMTLSEdge(ctx, edgeListener, runtimeMTLSEdgeConfig{
			UpstreamAddress: upstreamListener.Addr().String(),
			DialTimeout:     time.Second,
			IdleTimeout:     2 * time.Second,
			ShutdownTimeout: time.Second,
		}, (&net.Dialer{}).DialContext, &runtimeMTLSEdgeReporter{writer: &stderr})
	}()

	clientRaw, err := net.DialTimeout("tcp", edgeListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := clientRaw.(*net.TCPConn)
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatalf("response changed in transit: got %d bytes, want %d", len(response), len(payload))
	}
	select {
	case received := <-upstreamReceived:
		if !bytes.Equal(received, payload) {
			t.Fatalf("upstream payload changed in transit: got %d bytes, want %d", len(received), len(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive half-close")
	}
	if err := <-upstreamErr; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := waitRuntimeMTLSEdgeResult(t, done); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected edge diagnostics: %q", stderr.String())
	}
}

func TestRuntimeMTLSEdgeConnectionLimitRejectsBeforeUpstreamDialAndReleases(t *testing.T) {
	edgeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialCalls := make(chan struct{}, 4)
	upstreamPeers := make(chan net.Conn, 4)
	dial := func(context.Context, string, string) (net.Conn, error) {
		local, peer := net.Pipe()
		dialCalls <- struct{}{}
		upstreamPeers <- peer
		return local, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- serveRuntimeMTLSEdge(ctx, edgeListener, runtimeMTLSEdgeConfig{
			UpstreamAddress: "core-api:8443",
			DialTimeout:     time.Second,
			IdleTimeout:     5 * time.Second,
			ShutdownTimeout: time.Second,
			MaxConnections:  1,
		}, dial, &runtimeMTLSEdgeReporter{writer: &stderr})
	}()

	first, err := net.DialTimeout("tcp", edgeListener.Addr().String(), time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer first.Close()
	var firstUpstream net.Conn
	select {
	case <-dialCalls:
		firstUpstream = <-upstreamPeers
	case <-time.After(time.Second):
		cancel()
		t.Fatal("first connection did not dial upstream")
	}
	defer firstUpstream.Close()

	second, err := net.DialTimeout("tcp", edgeListener.Addr().String(), time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	if _, readErr := second.Read(make([]byte, 1)); readErr == nil {
		_ = second.Close()
		cancel()
		t.Fatal("over-limit downstream connection remained open")
	}
	_ = second.Close()
	select {
	case <-dialCalls:
		cancel()
		t.Fatal("over-limit connection reached the upstream dial")
	case <-time.After(100 * time.Millisecond):
	}

	_ = first.Close()
	_ = firstUpstream.Close()

	var replacement net.Conn
	var replacementUpstream net.Conn
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		candidate, dialErr := net.DialTimeout("tcp", edgeListener.Addr().String(), 100*time.Millisecond)
		if dialErr != nil {
			continue
		}
		select {
		case <-dialCalls:
			replacement = candidate
			replacementUpstream = <-upstreamPeers
		case <-time.After(20 * time.Millisecond):
			_ = candidate.Close()
		}
		if replacement != nil {
			break
		}
	}
	if replacement == nil || replacementUpstream == nil {
		cancel()
		t.Fatal("connection permit was not released after the first connection closed")
	}
	_ = replacement.Close()
	_ = replacementUpstream.Close()
	if !strings.Contains(stderr.String(), "connection limit reached") {
		cancel()
		t.Fatalf("connection admission did not report its bounded rejection: %q", stderr.String())
	}

	cancel()
	if err := waitRuntimeMTLSEdgeResult(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeMTLSEdgeCarriesMutualTLSOnlyToRuntimePaths(t *testing.T) {
	certFile, keyFile := writeRuntimeTestCertificate(t)
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)

	upstreamTCP, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstreamTLS := tls.NewListener(upstreamTCP, &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	})
	server := &http.Server{Handler: runtimeOnlyHandler(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/agent-runtime/ws" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(upstreamTLS) }()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		if serveErr := <-serverDone; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Errorf("Runtime mTLS server error: %v", serveErr)
		}
	}()

	edgeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	edgeCtx, edgeCancel := context.WithCancel(context.Background())
	edgeDone := make(chan error, 1)
	go func() {
		edgeDone <- serveRuntimeMTLSEdge(edgeCtx, edgeListener, runtimeMTLSEdgeConfig{
			UpstreamAddress: upstreamTCP.Addr().String(),
			DialTimeout:     time.Second,
			IdleTimeout:     time.Second,
			ShutdownTimeout: time.Second,
		}, (&net.Dialer{}).DialContext, &runtimeMTLSEdgeReporter{writer: io.Discard})
	}()
	defer func() {
		edgeCancel()
		if edgeErr := waitRuntimeMTLSEdgeResult(t, edgeDone); edgeErr != nil {
			t.Errorf("Runtime mTLS edge error: %v", edgeErr)
		}
	}()

	clientTLS := &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{pair},
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
	}
	requestStatus := func(path string) int {
		t.Helper()
		conn, dialErr := tls.Dial("tcp", edgeListener.Addr().String(), clientTLS)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, writeErr := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", path); writeErr != nil {
			t.Fatal(writeErr)
		}
		response, readErr := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
		if readErr != nil {
			t.Fatal(readErr)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := requestStatus("/api/v1/agent-runtime/ws"); status != http.StatusUnauthorized {
		t.Fatalf("Runtime protocol probe status = %d, want 401", status)
	}
	if status := requestStatus("/healthz"); status != http.StatusNotFound {
		t.Fatalf("non-Runtime path status = %d, want 404", status)
	}

	withoutCertificate := clientTLS.Clone()
	withoutCertificate.Certificates = nil
	if conn, dialErr := tls.Dial("tcp", edgeListener.Addr().String(), withoutCertificate); dialErr == nil {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, writeErr := fmt.Fprint(conn, "GET /api/v1/agent-runtime/ws HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
		if writeErr == nil {
			response, readErr := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
			if readErr == nil {
				_ = response.Body.Close()
				t.Fatal("Runtime mTLS edge accepted a certificate-free HTTP client")
			}
		}
	}
}

func TestRuntimeMTLSEdgeIdleTimeoutClosesStalledConnection(t *testing.T) {
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamListener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := upstreamListener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	edgeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- serveRuntimeMTLSEdge(ctx, edgeListener, runtimeMTLSEdgeConfig{
			UpstreamAddress: upstreamListener.Addr().String(),
			DialTimeout:     time.Second,
			IdleTimeout:     75 * time.Millisecond,
			ShutdownTimeout: time.Second,
		}, (&net.Dialer{}).DialContext, &runtimeMTLSEdgeReporter{writer: &stderr})
	}()
	client, err := net.DialTimeout("tcp", edgeListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var upstream net.Conn
	select {
	case upstream = <-accepted:
		defer upstream.Close()
	case <-time.After(time.Second):
		t.Fatal("upstream connection not accepted")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection remained open")
	}
	cancel()
	if err := waitRuntimeMTLSEdgeResult(t, done); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("idle timeout should not leak diagnostics: %q", stderr.String())
	}
}

func TestRuntimeMTLSEdgeDialTimeoutIsEnforcedAndSafe(t *testing.T) {
	const secret = "upstream-dial-secret"
	edgeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialStarted := make(chan time.Duration, 1)
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			dialStarted <- 0
		} else {
			dialStarted <- time.Until(deadline)
		}
		<-ctx.Done()
		return nil, errors.New(secret)
	}
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- serveRuntimeMTLSEdge(ctx, edgeListener, runtimeMTLSEdgeConfig{
			UpstreamAddress: "core-api:8443",
			DialTimeout:     60 * time.Millisecond,
			IdleTimeout:     time.Second,
			ShutdownTimeout: time.Second,
		}, dial, &runtimeMTLSEdgeReporter{writer: &stderr})
	}()
	client, err := net.DialTimeout("tcp", edgeListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case remaining := <-dialStarted:
		if remaining <= 0 || remaining > 60*time.Millisecond {
			t.Fatalf("dial deadline remaining = %s", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream dial did not start")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("downstream remained open after dial timeout")
	}
	cancel()
	if err := waitRuntimeMTLSEdgeResult(t, done); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "upstream dial failed") || strings.Contains(stderr.String(), secret) {
		t.Fatalf("unsafe dial diagnostic: %q", stderr.String())
	}
}

func TestRuntimeMTLSEdgeContextCancellationClosesActiveConnections(t *testing.T) {
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamListener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := upstreamListener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	edgeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveRuntimeMTLSEdge(ctx, edgeListener, runtimeMTLSEdgeConfig{
			UpstreamAddress: upstreamListener.Addr().String(),
			DialTimeout:     time.Second,
			IdleTimeout:     time.Minute,
			ShutdownTimeout: time.Second,
		}, (&net.Dialer{}).DialContext, &runtimeMTLSEdgeReporter{writer: io.Discard})
	}()
	client, err := net.DialTimeout("tcp", edgeListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var upstream net.Conn
	select {
	case upstream = <-accepted:
		defer upstream.Close()
	case <-time.After(time.Second):
		t.Fatal("upstream connection not accepted")
	}
	cancel()
	if err := waitRuntimeMTLSEdgeResult(t, done); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("active downstream remained open after cancellation")
	}
}

func TestRuntimeMTLSEdgeShutdownTimeoutIsBounded(t *testing.T) {
	edgeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	var once sync.Once
	dial := func(context.Context, string, string) (net.Conn, error) {
		once.Do(func() { close(dialStarted) })
		<-releaseDial
		return nil, context.Canceled
	}
	done := make(chan error, 1)
	go func() {
		done <- serveRuntimeMTLSEdge(ctx, edgeListener, runtimeMTLSEdgeConfig{
			UpstreamAddress: "core-api:8443",
			DialTimeout:     time.Second,
			IdleTimeout:     time.Second,
			ShutdownTimeout: 30 * time.Millisecond,
		}, dial, &runtimeMTLSEdgeReporter{writer: io.Discard})
	}()
	client, err := net.DialTimeout("tcp", edgeListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("upstream dial did not start")
	}
	cancel()
	if err := waitRuntimeMTLSEdgeResult(t, done); !errors.Is(err, errRuntimeMTLSEdgeShutdownTimeout) {
		t.Fatalf("serve error = %v, want shutdown timeout", err)
	}
	close(releaseDial)
}

func TestRunRuntimeMTLSEdgeHandlesSIGTERM(t *testing.T) {
	if os.Getenv("OPENLINKER_RUNTIME_EDGE_SIGNAL_HELPER") == "1" {
		code := runRuntimeMTLSEdge([]string{
			"--listen", os.Getenv("OPENLINKER_RUNTIME_EDGE_SIGNAL_LISTEN"),
			"--upstream", "127.0.0.1:1",
			"--idle-timeout", "1s",
		}, os.Getenv, os.Stdout, os.Stderr)
		os.Exit(code)
	}

	listenAddress := reserveRuntimeMTLSEdgeAddress(t)
	command := exec.Command(os.Args[0], "-test.run=^TestRunRuntimeMTLSEdgeHandlesSIGTERM$")
	command.Env = append(os.Environ(),
		"OPENLINKER_RUNTIME_EDGE_SIGNAL_HELPER=1",
		"OPENLINKER_RUNTIME_EDGE_SIGNAL_LISTEN="+listenAddress,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	ready := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "runtime-mtls-edge: ready\n" {
			_ = command.Process.Kill()
			t.Fatalf("unexpected helper readiness output %q, stderr=%q", line, stderr.String())
		}
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		t.Fatalf("helper did not become ready, stderr=%q", stderr.String())
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("SIGTERM helper failed: %v stderr=%q", err, stderr.String())
		}
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("SIGTERM helper did not exit")
	}
}

func waitRuntimeMTLSEdgeResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Runtime mTLS edge did not stop")
		return nil
	}
}

func reserveRuntimeMTLSEdgeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
