package auth

import (
	"context"

	"github.com/google/uuid"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
)

// UserStatusChecker keeps non-HTTP authentication paths aligned with the
// session middleware's deleted/disabled-user checks.
type UserStatusChecker interface {
	EnsureUserEnabled(context.Context, uuid.UUID) error
}

type DBUserStatusChecker struct {
	users userStatusQuerier
}

func NewDBUserStatusChecker(dbtx db.DBTX) *DBUserStatusChecker {
	return &DBUserStatusChecker{users: db.New(dbtx)}
}

func (c *DBUserStatusChecker) EnsureUserEnabled(ctx context.Context, userID uuid.UUID) error {
	if c == nil {
		return nil
	}
	return ensureTokenUserEnabled(ctx, c.users, userID.String())
}

var _ UserStatusChecker = (*DBUserStatusChecker)(nil)
