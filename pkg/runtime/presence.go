package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	defaultRuntimePresencePrefix = "openlinker:runtime:v2"
	minRuntimePresenceTTL        = time.Second
	maxRuntimePresenceTTL        = 5 * time.Minute
	runtimePresenceIndexTTL      = 10 * time.Minute
)

// RuntimePresence is an expiring routing/display hint. PostgreSQL Node,
// Session, attachment, certificate and credential state always wins.
type RuntimePresence struct {
	CoreInstanceID   uuid.UUID `json:"core_instance_id"`
	NodeID           uuid.UUID `json:"node_id"`
	AgentID          uuid.UUID `json:"agent_id"`
	RuntimeSessionID uuid.UUID `json:"runtime_session_id"`
	ConnectionID     string    `json:"connection_id"`
	WorkerID         string    `json:"worker_id"`
	Capacity         int32     `json:"capacity"`
	Inflight         int32     `json:"inflight"`
	NodeVersion      string    `json:"version"`
}

type RuntimePresenceStore interface {
	Refresh(context.Context, RuntimePresence, time.Duration) error
	ListByAgent(context.Context, uuid.UUID) ([]RuntimePresence, error)
	Remove(context.Context, RuntimePresence) error
}

type RedisRuntimePresenceStore struct {
	client redis.UniversalClient
	prefix string
}

func NewRedisRuntimePresenceStore(client redis.UniversalClient, prefix string) (*RedisRuntimePresenceStore, error) {
	if runtimeRedisClientUnavailable(client) {
		return nil, fmt.Errorf("%w: Redis client is required", ErrRuntimeSignalBusUnavailable)
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = defaultRuntimePresencePrefix
	}
	if len(prefix) > 120 || strings.ContainsAny(prefix, "{}\x00\r\n") {
		return nil, fmt.Errorf("runtime presence prefix is invalid")
	}
	return &RedisRuntimePresenceStore{client: client, prefix: prefix}, nil
}

func (s *RedisRuntimePresenceStore) Refresh(ctx context.Context, presence RuntimePresence, ttl time.Duration) error {
	if s == nil || runtimeRedisClientUnavailable(s.client) {
		return ErrRuntimeSignalBusUnavailable
	}
	if err := validateRuntimePresence(presence); err != nil {
		return err
	}
	if ttl < minRuntimePresenceTTL || ttl > maxRuntimePresenceTTL {
		return fmt.Errorf("runtime presence TTL must be between %s and %s", minRuntimePresenceTTL, maxRuntimePresenceTTL)
	}
	encoded, err := json.Marshal(presence)
	if err != nil {
		return fmt.Errorf("encode runtime presence: %w", err)
	}
	now, err := s.client.Time(ctx).Result()
	if err != nil {
		return fmt.Errorf("read Redis time for runtime presence: %w", err)
	}
	presenceKey := s.presenceKey(presence)
	indexKey := s.agentIndexKey(presence.AgentID)
	_, err = s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, presenceKey, encoded, ttl)
		pipe.ZAdd(ctx, indexKey, redis.Z{
			Score:  float64(now.Add(ttl).UnixMilli()),
			Member: presenceKey,
		})
		pipe.Expire(ctx, indexKey, runtimePresenceIndexTTL)
		return nil
	})
	if err != nil {
		return fmt.Errorf("refresh runtime presence: %w", err)
	}
	return nil
}

func (s *RedisRuntimePresenceStore) ListByAgent(ctx context.Context, agentID uuid.UUID) ([]RuntimePresence, error) {
	if s == nil || runtimeRedisClientUnavailable(s.client) {
		return nil, ErrRuntimeSignalBusUnavailable
	}
	if agentID == uuid.Nil {
		return nil, fmt.Errorf("runtime presence agent_id is required")
	}
	now, err := s.client.Time(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("read Redis time for runtime presence: %w", err)
	}
	indexKey := s.agentIndexKey(agentID)
	var keyCommand *redis.StringSliceCmd
	_, err = s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.ZRemRangeByScore(ctx, indexKey, "-inf", strconv.FormatInt(now.UnixMilli(), 10))
		keyCommand = pipe.ZRange(ctx, indexKey, 0, -1)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list runtime presence index: %w", err)
	}
	keys, err := keyCommand.Result()
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read runtime presence: %w", err)
	}

	presences := make([]RuntimePresence, 0, len(values))
	staleKeys := make([]interface{}, 0)
	var decodeErrors error
	for index, value := range values {
		if value == nil {
			staleKeys = append(staleKeys, keys[index])
			continue
		}
		encoded, ok := value.(string)
		if !ok {
			decodeErrors = errors.Join(decodeErrors, errors.New("runtime presence has an unexpected Redis value type"))
			staleKeys = append(staleKeys, keys[index])
			continue
		}
		presence, decodeErr := parseRuntimePresence([]byte(encoded))
		if decodeErr != nil || presence.AgentID != agentID {
			if decodeErr == nil {
				decodeErr = errors.New("runtime presence agent_id does not match its index")
			}
			decodeErrors = errors.Join(decodeErrors, decodeErr)
			staleKeys = append(staleKeys, keys[index])
			continue
		}
		presences = append(presences, presence)
	}
	if len(staleKeys) > 0 {
		if removeErr := s.client.ZRem(ctx, indexKey, staleKeys...).Err(); removeErr != nil {
			decodeErrors = errors.Join(decodeErrors, fmt.Errorf("remove stale runtime presence index entries: %w", removeErr))
		}
	}
	return presences, decodeErrors
}

func (s *RedisRuntimePresenceStore) Remove(ctx context.Context, presence RuntimePresence) error {
	if s == nil || runtimeRedisClientUnavailable(s.client) {
		return ErrRuntimeSignalBusUnavailable
	}
	if err := validateRuntimePresence(presence); err != nil {
		return err
	}
	key := s.presenceKey(presence)
	_, err := s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, key)
		pipe.ZRem(ctx, s.agentIndexKey(presence.AgentID), key)
		return nil
	})
	if err != nil {
		return fmt.Errorf("remove runtime presence: %w", err)
	}
	return nil
}

func (s *RedisRuntimePresenceStore) presenceKey(presence RuntimePresence) string {
	return fmt.Sprintf(
		"%s:presence:{%s}:%s:%s",
		s.prefix,
		presence.AgentID,
		presence.RuntimeSessionID,
		presence.CoreInstanceID,
	)
}

func (s *RedisRuntimePresenceStore) agentIndexKey(agentID uuid.UUID) string {
	return fmt.Sprintf("%s:presence:{%s}:index", s.prefix, agentID)
}

func validateRuntimePresence(presence RuntimePresence) error {
	if presence.CoreInstanceID == uuid.Nil || presence.NodeID == uuid.Nil ||
		presence.AgentID == uuid.Nil || presence.RuntimeSessionID == uuid.Nil {
		return errors.New("runtime presence identifiers are required")
	}
	if !validPresenceString(presence.ConnectionID, 200) ||
		!validPresenceString(presence.WorkerID, 200) ||
		!validPresenceString(presence.NodeVersion, 100) {
		return errors.New("runtime presence strings are invalid")
	}
	if presence.Capacity < 0 || presence.Capacity > RuntimeMaximumNodeCapacity ||
		presence.Inflight < 0 || presence.Inflight > RuntimeMaximumNodeCapacity {
		return errors.New("runtime presence capacity is invalid")
	}
	return nil
}

func validPresenceString(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func parseRuntimePresence(encoded []byte) (RuntimePresence, error) {
	var presence RuntimePresence
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&presence); err != nil {
		return RuntimePresence{}, errors.New("invalid runtime presence JSON")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RuntimePresence{}, errors.New("runtime presence must contain one JSON value")
	}
	if err := validateRuntimePresence(presence); err != nil {
		return RuntimePresence{}, err
	}
	return presence, nil
}

var _ RuntimePresenceStore = (*RedisRuntimePresenceStore)(nil)
