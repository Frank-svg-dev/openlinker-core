package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// LocalSignalBus is a single-process implementation for development, tests,
// and explicitly single-instance deployments. It does not claim HA delivery.
type LocalSignalBus struct {
	instanceID uuid.UUID

	mu          sync.RWMutex
	subscribers map[uint64]RuntimeSignalHandler
	nextID      uint64
	closed      bool
	done        chan struct{}
}

func NewLocalSignalBus(instanceID uuid.UUID) *LocalSignalBus {
	return &LocalSignalBus{
		instanceID:  instanceID,
		subscribers: make(map[uint64]RuntimeSignalHandler),
		done:        make(chan struct{}),
	}
}

func (b *LocalSignalBus) Publish(ctx context.Context, signal RuntimeSignal) error {
	if b == nil {
		return ErrRuntimeSignalBusUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateRuntimeSignal(signal); err != nil {
		return err
	}
	if !runtimeSignalTargetsInstance(signal, b.instanceID) {
		return nil
	}

	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return ErrRuntimeSignalBusClosed
	}
	handlers := make([]RuntimeSignalHandler, 0, len(b.subscribers))
	for _, handler := range b.subscribers {
		handlers = append(handlers, handler)
	}
	b.mu.RUnlock()

	var combined error
	for _, handler := range handlers {
		if err := ctx.Err(); err != nil {
			return errors.Join(combined, err)
		}
		if err := handler(ctx, signal); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

func (b *LocalSignalBus) Subscribe(ctx context.Context, handler RuntimeSignalHandler) error {
	if b == nil {
		return ErrRuntimeSignalBusUnavailable
	}
	if handler == nil {
		return fmt.Errorf("%w: handler is required", ErrRuntimeSignalInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrRuntimeSignalBusClosed
	}
	id := b.nextID
	b.nextID++
	b.subscribers[id] = handler
	done := b.done
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ErrRuntimeSignalBusClosed
	}
}

func (b *LocalSignalBus) Health(ctx context.Context) error {
	if b == nil {
		return ErrRuntimeSignalBusUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrRuntimeSignalBusClosed
	}
	return nil
}

func (b *LocalSignalBus) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.done)
	return nil
}

var _ RuntimeSignalBus = (*LocalSignalBus)(nil)
