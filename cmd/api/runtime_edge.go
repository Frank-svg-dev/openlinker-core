package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultRuntimeMTLSEdgeListenAddress   = ":8443"
	defaultRuntimeMTLSEdgeUpstreamAddress = "core-api:8443"

	defaultRuntimeMTLSEdgeDialTimeout     = 5 * time.Second
	defaultRuntimeMTLSEdgeIdleTimeout     = 2 * time.Minute
	defaultRuntimeMTLSEdgeShutdownTimeout = 10 * time.Second
	defaultRuntimeMTLSEdgeMaxConnections  = 4096
	minimumRuntimeMTLSEdgeMaxConnections  = 1
	maximumRuntimeMTLSEdgeMaxConnections  = 65535

	runtimeMTLSEdgeBufferSize = 32 * 1024
)

const runtimeMTLSEdgeUsage = `usage: api runtime-mtls-edge [flags]

Transparently forwards Runtime mTLS TCP connections without terminating TLS.

Flags:
  --listen address          listen address (default :8443)
  --upstream address        Core Runtime mTLS address (default core-api:8443)
  --dial-timeout duration   upstream TCP dial timeout (default 5s)
  --idle-timeout duration   per-direction no-progress timeout (default 2m)
  --shutdown-timeout duration
                            maximum shutdown wait (default 10s)
  --max-connections count   maximum concurrent downstream connections (default 4096)
  --healthcheck             TCP dial the local edge and Core upstream, then exit
`

var (
	errRuntimeMTLSEdgeInvalidFlags    = errors.New("invalid flags")
	errRuntimeMTLSEdgeAccept          = errors.New("accept failed")
	errRuntimeMTLSEdgeForward         = errors.New("forwarding failed")
	errRuntimeMTLSEdgeShutdownTimeout = errors.New("shutdown timed out")
	errRuntimeMTLSEdgeSelfCheck       = errors.New("upstream self-check failed")
)

type runtimeMTLSEdgeConfig struct {
	ListenAddress   string
	UpstreamAddress string
	DialTimeout     time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	MaxConnections  int
	Healthcheck     bool
}

type runtimeMTLSEdgeDialFunc func(context.Context, string, string) (net.Conn, error)

type runtimeMTLSEdgeDependencies struct {
	Listen         func(string, string) (net.Listener, error)
	Dial           runtimeMTLSEdgeDialFunc
	LookupIP       func(context.Context, string, string) ([]net.IP, error)
	InterfaceAddrs func() ([]net.Addr, error)
}

func runRuntimeMTLSEdge(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runRuntimeMTLSEdgeWithContext(ctx, args, getenv, stdout, stderr)
}

func runRuntimeMTLSEdgeWithContext(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	return runRuntimeMTLSEdgeWithDependencies(ctx, args, getenv, stdout, stderr, runtimeMTLSEdgeDependencies{})
}

func runRuntimeMTLSEdgeWithDependencies(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stdout io.Writer,
	stderr io.Writer,
	dependencies runtimeMTLSEdgeDependencies,
) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if dependencies.Listen == nil {
		dependencies.Listen = net.Listen
	}
	if dependencies.Dial == nil {
		dialer := &net.Dialer{KeepAlive: 30 * time.Second}
		dependencies.Dial = dialer.DialContext
	}
	if dependencies.LookupIP == nil {
		dependencies.LookupIP = net.DefaultResolver.LookupIP
	}
	if dependencies.InterfaceAddrs == nil {
		dependencies.InterfaceAddrs = net.InterfaceAddrs
	}

	cfg, err := parseRuntimeMTLSEdgeConfig(args, getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, runtimeMTLSEdgeUsage)
			return 0
		}
		fmt.Fprintln(stderr, "runtime-mtls-edge: invalid arguments")
		return 2
	}
	if ctx.Err() != nil {
		return 0
	}
	if cfg.Healthcheck {
		if !checkRuntimeMTLSEdgeHealth(ctx, cfg, dependencies.Dial) {
			if ctx.Err() != nil {
				return 0
			}
			fmt.Fprintln(stderr, "runtime-mtls-edge: healthcheck failed")
			return 1
		}
		return 0
	}

	listener, err := dependencies.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		fmt.Fprintln(stderr, "runtime-mtls-edge: listen failed")
		return 1
	}
	if err = validateRuntimeMTLSEdgeResolvedUpstream(
		ctx, listener, cfg, dependencies.LookupIP, dependencies.InterfaceAddrs,
	); err != nil {
		_ = listener.Close()
		fmt.Fprintln(stderr, "runtime-mtls-edge: upstream self-check failed")
		return 1
	}
	fmt.Fprintln(stdout, "runtime-mtls-edge: ready")
	reporter := &runtimeMTLSEdgeReporter{writer: stderr}
	if err := serveRuntimeMTLSEdge(ctx, listener, cfg, dependencies.Dial, reporter); err != nil {
		switch {
		case errors.Is(err, errRuntimeMTLSEdgeAccept):
			fmt.Fprintln(stderr, "runtime-mtls-edge: accept failed")
		case errors.Is(err, errRuntimeMTLSEdgeShutdownTimeout):
			fmt.Fprintln(stderr, "runtime-mtls-edge: shutdown timed out")
		default:
			fmt.Fprintln(stderr, "runtime-mtls-edge: serving failed")
		}
		return 1
	}
	return 0
}

func parseRuntimeMTLSEdgeConfig(args []string, getenv func(string) string) (runtimeMTLSEdgeConfig, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	cfg := runtimeMTLSEdgeConfig{
		ListenAddress:   runtimeMTLSEdgeEnvOrDefault(getenv, "OPENLINKER_RUNTIME_EDGE_LISTEN", defaultRuntimeMTLSEdgeListenAddress),
		UpstreamAddress: runtimeMTLSEdgeEnvOrDefault(getenv, "OPENLINKER_RUNTIME_EDGE_UPSTREAM", defaultRuntimeMTLSEdgeUpstreamAddress),
	}
	dialTimeout := runtimeMTLSEdgeEnvOrDefault(getenv, "OPENLINKER_RUNTIME_EDGE_DIAL_TIMEOUT", defaultRuntimeMTLSEdgeDialTimeout.String())
	idleTimeout := runtimeMTLSEdgeEnvOrDefault(getenv, "OPENLINKER_RUNTIME_EDGE_IDLE_TIMEOUT", defaultRuntimeMTLSEdgeIdleTimeout.String())
	shutdownTimeout := runtimeMTLSEdgeEnvOrDefault(getenv, "OPENLINKER_RUNTIME_EDGE_SHUTDOWN_TIMEOUT", defaultRuntimeMTLSEdgeShutdownTimeout.String())
	maxConnections := runtimeMTLSEdgeEnvOrDefault(
		getenv,
		"OPENLINKER_RUNTIME_EDGE_MAX_CONNECTIONS",
		strconv.Itoa(defaultRuntimeMTLSEdgeMaxConnections),
	)

	fs := flag.NewFlagSet("runtime-mtls-edge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.ListenAddress, "listen", cfg.ListenAddress, "listen address")
	fs.StringVar(&cfg.UpstreamAddress, "upstream", cfg.UpstreamAddress, "Core Runtime mTLS address")
	fs.StringVar(&dialTimeout, "dial-timeout", dialTimeout, "upstream TCP dial timeout")
	fs.StringVar(&idleTimeout, "idle-timeout", idleTimeout, "per-direction no-progress timeout")
	fs.StringVar(&shutdownTimeout, "shutdown-timeout", shutdownTimeout, "maximum shutdown wait")
	fs.StringVar(&maxConnections, "max-connections", maxConnections, "maximum concurrent downstream connections")
	fs.BoolVar(&cfg.Healthcheck, "healthcheck", false, "TCP dial the local edge and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return runtimeMTLSEdgeConfig{}, flag.ErrHelp
		}
		return runtimeMTLSEdgeConfig{}, errRuntimeMTLSEdgeInvalidFlags
	}
	if fs.NArg() != 0 {
		return runtimeMTLSEdgeConfig{}, errRuntimeMTLSEdgeInvalidFlags
	}

	var err error
	if cfg.DialTimeout, err = parseRuntimeMTLSEdgeDuration(dialTimeout, time.Millisecond, time.Minute); err != nil {
		return runtimeMTLSEdgeConfig{}, errRuntimeMTLSEdgeInvalidFlags
	}
	if cfg.IdleTimeout, err = parseRuntimeMTLSEdgeDuration(idleTimeout, time.Millisecond, 24*time.Hour); err != nil {
		return runtimeMTLSEdgeConfig{}, errRuntimeMTLSEdgeInvalidFlags
	}
	if cfg.ShutdownTimeout, err = parseRuntimeMTLSEdgeDuration(shutdownTimeout, time.Millisecond, time.Minute); err != nil {
		return runtimeMTLSEdgeConfig{}, errRuntimeMTLSEdgeInvalidFlags
	}
	if cfg.MaxConnections, err = parseRuntimeMTLSEdgeConnectionLimit(maxConnections); err != nil {
		return runtimeMTLSEdgeConfig{}, errRuntimeMTLSEdgeInvalidFlags
	}

	cfg.ListenAddress = strings.TrimSpace(cfg.ListenAddress)
	cfg.UpstreamAddress = strings.TrimSpace(cfg.UpstreamAddress)
	if !validRuntimeMTLSEdgeAddress(cfg.ListenAddress, true) ||
		!validRuntimeMTLSEdgeAddress(cfg.UpstreamAddress, false) ||
		runtimeMTLSEdgeSelfLoop(cfg.ListenAddress, cfg.UpstreamAddress) {
		return runtimeMTLSEdgeConfig{}, errRuntimeMTLSEdgeInvalidFlags
	}
	return cfg, nil
}

func checkRuntimeMTLSEdgeHealth(ctx context.Context, cfg runtimeMTLSEdgeConfig, dial runtimeMTLSEdgeDialFunc) bool {
	if dial == nil {
		return false
	}
	localAddress, ok := runtimeMTLSEdgeHealthcheckTarget(cfg.ListenAddress)
	if !ok {
		return false
	}
	healthy := true
	for _, address := range []string{localAddress, cfg.UpstreamAddress} {
		dialCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
		conn, err := dial(dialCtx, "tcp", address)
		cancel()
		if err != nil || conn == nil {
			healthy = false
			continue
		}
		_ = conn.Close()
	}
	return healthy
}

func runtimeMTLSEdgeHealthcheckTarget(listenAddress string) (string, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddress))
	if err != nil {
		return "", false
	}
	host = runtimeMTLSEdgeCanonicalHost(host)
	if runtimeMTLSEdgeWildcardHost(host) {
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			host = "::1"
		} else {
			host = "127.0.0.1"
		}
	}
	return net.JoinHostPort(host, port), true
}

func runtimeMTLSEdgeEnvOrDefault(getenv func(string) string, key, fallback string) string {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value
	}
	return fallback
}

func parseRuntimeMTLSEdgeDuration(value string, minimum, maximum time.Duration) (time.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, errRuntimeMTLSEdgeInvalidFlags
	}
	return parsed, nil
}

func parseRuntimeMTLSEdgeConnectionLimit(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < minimumRuntimeMTLSEdgeMaxConnections || parsed > maximumRuntimeMTLSEdgeMaxConnections {
		return 0, errRuntimeMTLSEdgeInvalidFlags
	}
	return parsed, nil
}

func validRuntimeMTLSEdgeAddress(address string, allowEmptyHost bool) bool {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || (!allowEmptyHost && host == "") {
		return false
	}
	if strings.ContainsAny(host, " \t\r\n\x00/?#@") {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port >= 1 && port <= 65535
}

func runtimeMTLSEdgeSelfLoop(listenAddress, upstreamAddress string) bool {
	listenHost, listenPort, listenErr := net.SplitHostPort(listenAddress)
	upstreamHost, upstreamPort, upstreamErr := net.SplitHostPort(upstreamAddress)
	if listenErr != nil || upstreamErr != nil || listenPort != upstreamPort {
		return false
	}
	listenHost = runtimeMTLSEdgeCanonicalHost(listenHost)
	upstreamHost = runtimeMTLSEdgeCanonicalHost(upstreamHost)
	if listenHost == upstreamHost {
		return true
	}
	if runtimeMTLSEdgeWildcardHost(listenHost) && runtimeMTLSEdgeLocalHost(upstreamHost) {
		return true
	}
	if runtimeMTLSEdgeWildcardHost(upstreamHost) && runtimeMTLSEdgeLocalHost(listenHost) {
		return true
	}
	return runtimeMTLSEdgeLoopbackHost(listenHost) && runtimeMTLSEdgeLoopbackHost(upstreamHost)
}

func runtimeMTLSEdgeCanonicalHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func runtimeMTLSEdgeWildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func runtimeMTLSEdgeLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func runtimeMTLSEdgeLocalHost(host string) bool {
	return runtimeMTLSEdgeWildcardHost(host) || runtimeMTLSEdgeLoopbackHost(host)
}

func validateRuntimeMTLSEdgeResolvedUpstream(
	parent context.Context,
	listener net.Listener,
	cfg runtimeMTLSEdgeConfig,
	lookupIP func(context.Context, string, string) ([]net.IP, error),
	interfaceAddrs func() ([]net.Addr, error),
) error {
	if listener == nil || listener.Addr() == nil || lookupIP == nil || interfaceAddrs == nil {
		return errRuntimeMTLSEdgeSelfCheck
	}
	listenerHost, listenerPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return errRuntimeMTLSEdgeSelfCheck
	}
	upstreamHost, upstreamPort, err := net.SplitHostPort(cfg.UpstreamAddress)
	if err != nil {
		return errRuntimeMTLSEdgeSelfCheck
	}
	if listenerPort != upstreamPort {
		return nil
	}

	listenerIP := net.ParseIP(runtimeMTLSEdgeCanonicalHost(listenerHost))
	if listenerIP == nil {
		return errRuntimeMTLSEdgeSelfCheck
	}
	localIPs := make(map[string]struct{})
	if listenerIP.IsUnspecified() {
		addresses, addressErr := interfaceAddrs()
		if addressErr != nil {
			return errRuntimeMTLSEdgeSelfCheck
		}
		for _, address := range addresses {
			if ip := runtimeMTLSEdgeIPFromAddress(address); ip != nil {
				localIPs[runtimeMTLSEdgeIPKey(ip)] = struct{}{}
			}
		}
		localIPs[runtimeMTLSEdgeIPKey(net.ParseIP("127.0.0.1"))] = struct{}{}
		localIPs[runtimeMTLSEdgeIPKey(net.ParseIP("::1"))] = struct{}{}
	} else {
		localIPs[runtimeMTLSEdgeIPKey(listenerIP)] = struct{}{}
	}

	resolveCtx, cancel := context.WithTimeout(parent, cfg.DialTimeout)
	defer cancel()
	upstreamIPs, err := runtimeMTLSEdgeResolveHost(resolveCtx, upstreamHost, lookupIP)
	if err != nil || len(upstreamIPs) == 0 {
		return errRuntimeMTLSEdgeSelfCheck
	}
	for _, upstreamIP := range upstreamIPs {
		if _, self := localIPs[runtimeMTLSEdgeIPKey(upstreamIP)]; self {
			return errRuntimeMTLSEdgeSelfCheck
		}
	}
	return nil
}

func runtimeMTLSEdgeResolveHost(
	ctx context.Context,
	host string,
	lookupIP func(context.Context, string, string) ([]net.IP, error),
) ([]net.IP, error) {
	host = runtimeMTLSEdgeCanonicalHost(host)
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return lookupIP(ctx, "ip", host)
}

func runtimeMTLSEdgeIPFromAddress(address net.Addr) net.IP {
	if address == nil {
		return nil
	}
	switch typed := address.(type) {
	case *net.IPNet:
		return typed.IP
	case *net.IPAddr:
		return typed.IP
	}
	host, _, err := net.SplitHostPort(address.String())
	if err == nil {
		return net.ParseIP(host)
	}
	ip, _, err := net.ParseCIDR(address.String())
	if err != nil {
		return nil
	}
	return ip
}

func runtimeMTLSEdgeIPKey(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return string(ipv4)
	}
	if ipv6 := ip.To16(); ipv6 != nil {
		return string(ipv6)
	}
	return ""
}

func serveRuntimeMTLSEdge(
	parent context.Context,
	listener net.Listener,
	cfg runtimeMTLSEdgeConfig,
	dial runtimeMTLSEdgeDialFunc,
	reporter *runtimeMTLSEdgeReporter,
) error {
	if parent == nil {
		parent = context.Background()
	}
	if listener == nil {
		return errRuntimeMTLSEdgeAccept
	}
	if dial == nil {
		dialer := &net.Dialer{KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	ctx, cancel := context.WithCancel(parent)
	connections := newRuntimeMTLSEdgeConnections()
	maxConnections := cfg.MaxConnections
	if maxConnections < minimumRuntimeMTLSEdgeMaxConnections || maxConnections > maximumRuntimeMTLSEdgeMaxConnections {
		maxConnections = defaultRuntimeMTLSEdgeMaxConnections
	}
	connectionPermits := make(chan struct{}, maxConnections)
	var handlers sync.WaitGroup

	listenerWatcherDone := make(chan struct{})
	go func() {
		defer close(listenerWatcherDone)
		<-ctx.Done()
		_ = listener.Close()
		connections.closeAll()
	}()

	var serveErr error
	for {
		downstream, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				serveErr = errRuntimeMTLSEdgeAccept
			}
			break
		}
		select {
		case connectionPermits <- struct{}{}:
		default:
			_ = downstream.Close()
			reporter.connectionLimitReached()
			continue
		}
		if !connections.add(downstream) {
			<-connectionPermits
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-connectionPermits }()
			handleRuntimeMTLSEdgeConnection(ctx, downstream, cfg, dial, connections, reporter)
		}()
	}

	cancel()
	connections.closeAll()
	<-listenerWatcherDone
	handlersDone := make(chan struct{})
	go func() {
		handlers.Wait()
		close(handlersDone)
	}()
	timer := time.NewTimer(cfg.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-handlersDone:
		return serveErr
	case <-timer.C:
		connections.closeAll()
		return errRuntimeMTLSEdgeShutdownTimeout
	}
}

func handleRuntimeMTLSEdgeConnection(
	ctx context.Context,
	downstream net.Conn,
	cfg runtimeMTLSEdgeConfig,
	dial runtimeMTLSEdgeDialFunc,
	connections *runtimeMTLSEdgeConnections,
	reporter *runtimeMTLSEdgeReporter,
) {
	defer func() {
		connections.remove(downstream)
		_ = downstream.Close()
	}()

	dialCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	upstream, err := dial(dialCtx, "tcp", cfg.UpstreamAddress)
	cancel()
	if err != nil || upstream == nil {
		if ctx.Err() == nil {
			reporter.dialFailure()
		}
		return
	}
	if !connections.add(upstream) {
		return
	}
	defer func() {
		connections.remove(upstream)
		_ = upstream.Close()
	}()

	if err := proxyRuntimeMTLSEdgeConnection(ctx, downstream, upstream, cfg.IdleTimeout); err != nil && ctx.Err() == nil {
		reporter.forwardFailure()
	}
}

func proxyRuntimeMTLSEdgeConnection(ctx context.Context, downstream, upstream net.Conn, idleTimeout time.Duration) error {
	stopContextClose := context.AfterFunc(ctx, func() {
		_ = downstream.Close()
		_ = upstream.Close()
	})
	defer stopContextClose()

	results := make(chan error, 2)
	go func() {
		results <- copyRuntimeMTLSEdgeDirection(upstream, downstream, idleTimeout)
	}()
	go func() {
		results <- copyRuntimeMTLSEdgeDirection(downstream, upstream, idleTimeout)
	}()

	first := <-results
	if first != nil {
		_ = downstream.Close()
		_ = upstream.Close()
	}
	second := <-results
	if ctx.Err() != nil || runtimeMTLSEdgeExpectedConnectionError(first) && runtimeMTLSEdgeExpectedConnectionError(second) {
		return nil
	}
	return errRuntimeMTLSEdgeForward
}

func copyRuntimeMTLSEdgeDirection(destination, source net.Conn, idleTimeout time.Duration) error {
	defer closeRuntimeMTLSEdgeWrite(destination)
	defer closeRuntimeMTLSEdgeRead(source)

	buffer := make([]byte, runtimeMTLSEdgeBufferSize)
	for {
		if err := source.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			return err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			if err := destination.SetWriteDeadline(time.Now().Add(idleTimeout)); err != nil {
				return err
			}
			remaining := buffer[:read]
			for len(remaining) > 0 {
				written, writeErr := destination.Write(remaining)
				if written > 0 {
					remaining = remaining[written:]
				}
				if writeErr != nil {
					return writeErr
				}
				if written == 0 {
					return io.ErrNoProgress
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func closeRuntimeMTLSEdgeWrite(conn net.Conn) {
	if closeWriter, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = closeWriter.CloseWrite()
	}
}

func closeRuntimeMTLSEdgeRead(conn net.Conn) {
	if closeReader, ok := conn.(interface{ CloseRead() error }); ok {
		_ = closeReader.CloseRead()
	}
}

func runtimeMTLSEdgeExpectedConnectionError(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

type runtimeMTLSEdgeConnections struct {
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]struct{}
}

func newRuntimeMTLSEdgeConnections() *runtimeMTLSEdgeConnections {
	return &runtimeMTLSEdgeConnections{connections: make(map[net.Conn]struct{})}
}

func (connections *runtimeMTLSEdgeConnections) add(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	connections.mu.Lock()
	if connections.closed {
		connections.mu.Unlock()
		_ = conn.Close()
		return false
	}
	connections.connections[conn] = struct{}{}
	connections.mu.Unlock()
	return true
}

func (connections *runtimeMTLSEdgeConnections) remove(conn net.Conn) {
	connections.mu.Lock()
	delete(connections.connections, conn)
	connections.mu.Unlock()
}

func (connections *runtimeMTLSEdgeConnections) closeAll() {
	connections.mu.Lock()
	connections.closed = true
	toClose := make([]net.Conn, 0, len(connections.connections))
	for conn := range connections.connections {
		toClose = append(toClose, conn)
		delete(connections.connections, conn)
	}
	connections.mu.Unlock()
	for _, conn := range toClose {
		_ = conn.Close()
	}
}

type runtimeMTLSEdgeReporter struct {
	writer io.Writer
	mu     sync.Mutex
	dial   sync.Once
	copy   sync.Once
	limit  sync.Once
}

func (reporter *runtimeMTLSEdgeReporter) dialFailure() {
	if reporter == nil {
		return
	}
	reporter.dial.Do(func() {
		reporter.write("runtime-mtls-edge: upstream dial failed")
	})
}

func (reporter *runtimeMTLSEdgeReporter) forwardFailure() {
	if reporter == nil {
		return
	}
	reporter.copy.Do(func() {
		reporter.write("runtime-mtls-edge: connection forwarding failed")
	})
}

func (reporter *runtimeMTLSEdgeReporter) connectionLimitReached() {
	if reporter == nil {
		return
	}
	reporter.limit.Do(func() {
		reporter.write("runtime-mtls-edge: connection limit reached")
	})
}

func (reporter *runtimeMTLSEdgeReporter) write(message string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if reporter.writer == nil {
		return
	}
	fmt.Fprintln(reporter.writer, message)
}
