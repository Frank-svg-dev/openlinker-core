// Command runtime-cutover performs explicit, CAS-protected Runtime v2
// maintenance transitions. It is intentionally separate from the API server:
// browsers can inspect maintenance evidence but cannot mutate cluster mode.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/OpenLinker-ai/openlinker-core/pkg/cutover"
	openlinkerruntime "github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitBlocked = 3
)

type commandConfig struct {
	databaseURL      string
	releaseID        string
	gitSHA           string
	signalMode       string
	redisURL         string
	requireExclusive bool
	requireNoMembers bool
	expectedVersion  int64
	expectedReplicas int
	cutoverID        string
	drainDeadline    time.Duration
	allowPreV2Noop   bool
	commandTimeout   time.Duration
}

type redisHealth struct {
	client *redis.Client
}

func (h redisHealth) Health(ctx context.Context) error {
	if h.client == nil {
		return errors.New("redis unavailable")
	}
	return h.client.Ping(ctx).Err()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	command := args[0]
	if command == "help" || command == "-h" || command == "--help" {
		usage(stdout)
		return exitOK
	}
	switch command {
	case "status", "preflight", "drain", "hard-maintenance", "reopen":
	default:
		usage(stderr)
		return exitUsage
	}
	cfg, parseErr := parseCommand(command, args[1:], stderr, getenv)
	if parseErr != nil {
		writeError(stderr, "invalid_arguments")
		return exitUsage
	}
	if strings.TrimSpace(cfg.databaseURL) == "" {
		writeError(stderr, cutover.BlockerDatabaseUnavailable)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.commandTimeout)
	defer cancel()
	poolConfig, err := pgxpool.ParseConfig(cfg.databaseURL)
	if err != nil {
		writeError(stderr, cutover.BlockerDatabaseUnavailable)
		return exitFailure
	}
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "openlinker-runtime-cutover"
	poolConfig.MaxConns = 1
	poolConfig.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		writeError(stderr, cutover.BlockerDatabaseUnavailable)
		return exitFailure
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		writeError(stderr, cutover.BlockerDatabaseUnavailable)
		return exitFailure
	}
	var signalHealth cutover.SignalHealth
	var redisClient *redis.Client
	if cfg.signalMode == "redis" {
		redisOptions, parseRedisErr := redis.ParseURL(cfg.redisURL)
		if parseRedisErr == nil {
			redisClient = redis.NewClient(redisOptions)
			signalHealth = redisHealth{client: redisClient}
			defer func() { _ = redisClient.Close() }()
		}
	}
	service := cutover.NewService(pool, cutover.ServiceConfig{
		Identity: cutover.Identity{
			ReleaseID: cfg.releaseID, GitSHA: cfg.gitSHA,
			SchemaVersion:         openlinkerruntime.RuntimeSchemaVersion,
			SchemaChecksum:        openlinkerruntime.RuntimeSchemaChecksum,
			MigrationName:         openlinkerruntime.RuntimeSchemaMigrationName,
			RuntimeContractID:     openlinkerruntime.RuntimeContractID,
			RuntimeContractDigest: openlinkerruntime.RuntimeContractDigest,
		},
		SignalMode: cfg.signalMode, SignalHealth: signalHealth,
	})
	request := cutover.TransitionRequest{
		ExpectedVersion: cfg.expectedVersion,
		AllowPreV2Noop:  cfg.allowPreV2Noop,
	}
	if cfg.expectedReplicas > 0 {
		request.ExpectedReplicas = int32(cfg.expectedReplicas)
	}
	if cfg.cutoverID != "" {
		request.CutoverID, err = uuid.Parse(cfg.cutoverID)
		if err != nil {
			writeError(stderr, cutover.BlockerCutoverIDMismatch)
			return exitUsage
		}
	}
	if cfg.drainDeadline > 0 {
		deadline := time.Now().UTC().Add(cfg.drainDeadline)
		request.DrainDeadline = &deadline
	}
	var report cutover.Report
	switch command {
	case "status":
		report, err = service.Status(ctx)
	case "preflight":
		report, err = service.Preflight(ctx, cutover.PreflightOptions{
			RequireExclusive: cfg.requireExclusive,
			RequireNoMembers: cfg.requireNoMembers,
		})
		if err == nil && !report.Readiness.Ready {
			err = &cutover.OperationError{Code: report.Readiness.Blockers[0].Code}
		}
	case "drain":
		report, err = service.Drain(ctx, request)
	case "hard-maintenance":
		report, err = service.HardMaintenance(ctx, request)
	case "reopen":
		report, err = service.Reopen(ctx, request)
	default:
		usage(stderr)
		return exitUsage
	}
	if err != nil {
		if report.DatabaseTime.IsZero() {
			writeError(stderr, cutover.ErrorCode(err))
		} else {
			_ = writeJSON(stderr, report)
		}
		if cutover.IsBlocked(err) {
			return exitBlocked
		}
		return exitFailure
	}
	if err = writeJSON(stdout, report); err != nil {
		writeError(stderr, "output_failed")
		return exitFailure
	}
	return exitOK
}

func parseCommand(command string, args []string, stderr io.Writer, getenv func(string) string) (commandConfig, error) {
	cfg := commandConfig{
		databaseURL: getenv("DATABASE_URL"), releaseID: getenv("OPENLINKER_RELEASE_ID"),
		gitSHA: getenv("OPENLINKER_GIT_SHA"), redisURL: envOr(getenv, "REDIS_URL", "redis://localhost:6379/0"),
		signalMode: "local", commandTimeout: durationEnv(getenv, "OPENLINKER_CUTOVER_COMMAND_TIMEOUT", 2*time.Minute),
	}
	if value, _ := strconv.ParseBool(getenv("RUNTIME_HA_MODE")); value {
		cfg.signalMode = "redis"
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.databaseURL, "database-url", cfg.databaseURL, "Postgres URL; defaults to DATABASE_URL")
	fs.StringVar(&cfg.releaseID, "release-id", cfg.releaseID, "target release identity; defaults to OPENLINKER_RELEASE_ID")
	fs.StringVar(&cfg.gitSHA, "git-sha", cfg.gitSHA, "target commit identity; defaults to OPENLINKER_GIT_SHA")
	fs.StringVar(&cfg.signalMode, "signal-mode", cfg.signalMode, "local or redis")
	fs.StringVar(&cfg.redisURL, "redis-url", cfg.redisURL, "Redis URL used for HA reopen evidence")
	fs.DurationVar(&cfg.commandTimeout, "timeout", cfg.commandTimeout, "overall command timeout")
	switch command {
	case "status":
	case "preflight":
		fs.BoolVar(&cfg.requireExclusive, "require-exclusive", false, "block while another database client is connected")
		fs.BoolVar(&cfg.requireNoMembers, "require-no-members", false, "block while any cluster membership row remains")
	case "drain":
		fs.Int64Var(&cfg.expectedVersion, "expected-version", 0, "required runtime_cluster_control CAS version")
		fs.IntVar(&cfg.expectedReplicas, "expected-replicas", 0, "required exact Core replica count")
		fs.DurationVar(&cfg.drainDeadline, "deadline", 0, "optional drain deadline duration")
		fs.BoolVar(&cfg.allowPreV2Noop, "pre-v2-ok", false, "return an explicit no-op when runtime v2 schema is not installed")
	case "hard-maintenance":
		fs.Int64Var(&cfg.expectedVersion, "expected-version", 0, "required runtime_cluster_control CAS version")
		fs.BoolVar(&cfg.allowPreV2Noop, "pre-v2-ok", false, "return an explicit no-op when migration 063 will create hard maintenance")
	case "reopen":
		fs.Int64Var(&cfg.expectedVersion, "expected-version", 0, "required runtime_cluster_control CAS version")
		fs.StringVar(&cfg.cutoverID, "cutover-id", "", "required cutover identity")
	default:
		return commandConfig{}, errors.New("unknown command")
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return commandConfig{}, errors.New("invalid arguments")
	}
	cfg.signalMode = strings.ToLower(strings.TrimSpace(cfg.signalMode))
	if cfg.signalMode != "local" && cfg.signalMode != "redis" {
		return commandConfig{}, errors.New("invalid signal mode")
	}
	if cfg.commandTimeout <= 0 {
		return commandConfig{}, errors.New("timeout must be positive")
	}
	if command == "drain" && (cfg.expectedVersion < 1 || cfg.expectedReplicas < 1) && !cfg.allowPreV2Noop {
		return commandConfig{}, errors.New("drain requires CAS version and replicas")
	}
	if (command == "hard-maintenance" || command == "reopen") && cfg.expectedVersion < 1 && !cfg.allowPreV2Noop {
		return commandConfig{}, errors.New("transition requires CAS version")
	}
	if command == "reopen" && cfg.cutoverID == "" {
		return commandConfig{}, errors.New("reopen requires cutover id")
	}
	return cfg, nil
}

func envOr(getenv func(string) string, key, fallback string) string {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(getenv func(string) string, key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(true)
	return encoder.Encode(value)
}

func writeError(writer io.Writer, code string) {
	_ = writeJSON(writer, map[string]any{
		"readiness": map[string]any{
			"ready":    false,
			"blockers": []cutover.Blocker{{Code: code, Scope: "command"}},
		},
	})
}

func usage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: runtime-cutover <status|preflight|drain|hard-maintenance|reopen> [flags]")
}
