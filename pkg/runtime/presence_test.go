package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRedisRuntimePresenceIsTTLBoundAdvisoryData(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisRuntimePresenceStore(client, "test:runtime")
	require.NoError(t, err)

	presence := validTestRuntimePresence()
	require.NoError(t, store.Refresh(context.Background(), presence, 2*time.Second))
	listed, err := store.ListByAgent(context.Background(), presence.AgentID)
	require.NoError(t, err)
	require.Equal(t, []RuntimePresence{presence}, listed)

	keys := server.Keys()
	sort.Strings(keys)
	require.Len(t, keys, 2)
	var encoded string
	for _, key := range keys {
		if server.Type(key) == "string" {
			encoded, err = server.Get(key)
			require.NoError(t, err)
		}
	}
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(encoded), &fields))
	for _, forbidden := range []string{"payload", "input", "output", "token", "secret", "capability"} {
		require.NotContains(t, fields, forbidden)
	}

	server.FastForward(3 * time.Second)
	listed, err = store.ListByAgent(context.Background(), presence.AgentID)
	require.NoError(t, err)
	require.Empty(t, listed, "expired Redis presence must not imply durable online state")
}

func TestRedisRuntimePresenceRemoveAndValidation(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisRuntimePresenceStore(client, "")
	require.NoError(t, err)

	presence := validTestRuntimePresence()
	require.NoError(t, store.Refresh(context.Background(), presence, 10*time.Second))
	require.NoError(t, store.Remove(context.Background(), presence))
	listed, err := store.ListByAgent(context.Background(), presence.AgentID)
	require.NoError(t, err)
	require.Empty(t, listed)

	draining := presence
	draining.Capacity = 0
	draining.Inflight = 1
	require.NoError(t, store.Refresh(context.Background(), draining, 10*time.Second),
		"a draining Node can advertise zero new capacity while work is still inflight")
	invalid := presence
	invalid.Inflight = RuntimeMaximumNodeCapacity + 1
	require.Error(t, store.Refresh(context.Background(), invalid, 10*time.Second))
	require.Error(t, store.Refresh(context.Background(), presence, time.Hour))
	_, err = store.ListByAgent(context.Background(), uuid.Nil)
	require.Error(t, err)
}

func TestRedisRuntimePresenceOldConnectionCannotRemoveReplacement(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	store, err := NewRedisRuntimePresenceStore(client, "test:runtime")
	require.NoError(t, err)

	oldConnection := validTestRuntimePresence()
	oldConnection.ConnectionID = "ws:old"
	replacement := oldConnection
	replacement.ConnectionID = "ws:new"
	require.NoError(t, store.Refresh(context.Background(), oldConnection, time.Minute))
	require.NoError(t, store.Refresh(context.Background(), replacement, time.Minute))
	require.NoError(t, store.Remove(context.Background(), oldConnection))

	listed, err := store.ListByAgent(context.Background(), replacement.AgentID)
	require.NoError(t, err)
	require.Equal(t, []RuntimePresence{replacement}, listed)
}

func TestRedisRuntimePresenceLossIsAdvisory(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: server.Addr(), DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond, WriteTimeout: 50 * time.Millisecond,
		MaxRetries: -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisRuntimePresenceStore(client, "test:runtime")
	require.NoError(t, err)
	server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.Error(t, store.Refresh(ctx, validTestRuntimePresence(), time.Minute))
}

func TestRedisRuntimePresenceRejectsCorruptHintWithoutTrustingIt(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisRuntimePresenceStore(client, "test:runtime")
	require.NoError(t, err)
	presence := validTestRuntimePresence()
	require.NoError(t, store.Refresh(context.Background(), presence, time.Minute))

	key := store.presenceKey(presence)
	require.NoError(t, client.Set(context.Background(), key, `{"agent_id":"classified","token":"secret"}`, time.Minute).Err())
	listed, listErr := store.ListByAgent(context.Background(), presence.AgentID)
	require.Error(t, listErr)
	require.Empty(t, listed)
	memberCount, err := client.ZCard(context.Background(), store.agentIndexKey(presence.AgentID)).Result()
	require.NoError(t, err)
	require.Zero(t, memberCount)
}

func validTestRuntimePresence() RuntimePresence {
	return RuntimePresence{
		CoreInstanceID: uuid.New(), NodeID: uuid.New(), AgentID: uuid.New(),
		RuntimeSessionID: uuid.New(), ConnectionID: "connection-1", WorkerID: "worker-1",
		Capacity: 4, Inflight: 1, NodeVersion: "0.2.0",
		Transport: RuntimeTransportWebSocket, TransportReason: RuntimeTransportReasonExplicit,
		TransportChangedAt: time.Now().UTC(),
	}
}
