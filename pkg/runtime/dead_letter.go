package runtime

import (
	"context"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

const (
	defaultDeadLetterLimit int32 = 50
	maxDeadLetterLimit     int32 = 200
)

// ListRuntimeDeadLetters returns the admin operational inventory without Run
// input/output or credential-linked execution identity.
func (s *Service) ListRuntimeDeadLetters(
	ctx context.Context,
	limit, offset int32,
) (*RuntimeDeadLetterListResponse, error) {
	if limit <= 0 {
		limit = defaultDeadLetterLimit
	}
	if limit > maxDeadLetterLimit {
		limit = maxDeadLetterLimit
	}
	if offset < 0 {
		return nil, httpx.BadRequest("offset 不能小于 0")
	}
	rows, err := s.queries.ListRunDeadLetters(ctx, db.ListRunDeadLettersParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, httpx.Internal("查询死信队列失败")
	}
	total, err := s.queries.CountRunDeadLetters(ctx)
	if err != nil {
		return nil, httpx.Internal("查询死信队列失败")
	}
	items := make([]RuntimeDeadLetterListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, runtimeDeadLetterListItem(row))
	}
	return &RuntimeDeadLetterListResponse{
		Items:  items,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}, nil
}

func runtimeDeadLetterListItem(row db.ListRunDeadLettersRow) RuntimeDeadLetterListItem {
	item := RuntimeDeadLetterListItem{
		DeadLetterID:     row.DeadLetterID.String(),
		RunID:            row.RunID.String(),
		AgentID:          row.AgentID.String(),
		AgentSlug:        row.AgentSlug,
		AgentName:        row.AgentName,
		Status:           row.Status,
		DispatchState:    row.DispatchState,
		AttemptCount:     row.AttemptCount,
		MaxAttempts:      row.MaxAttempts,
		FinalAttemptNo:   row.FinalAttemptNo,
		ReasonCode:       row.ReasonCode,
		DeadLetteredAt:   row.DeadLetteredAt,
		CreatedAt:        row.CreatedAt,
		ReplayedAsRunIDs: make([]string, 0, len(row.ReplayedAsRunIDs)),
	}
	if row.FinalAttemptID != nil {
		item.FinalAttemptID = row.FinalAttemptID.String()
	}
	if row.ErrorCode != nil {
		item.ErrorCode = *row.ErrorCode
	}
	if row.ErrorMessage != nil {
		item.ErrorMessage = *row.ErrorMessage
	}
	if row.ErrorDetailRedacted != nil {
		item.ErrorDetail = *row.ErrorDetailRedacted
	}
	if row.ReasonRedacted != nil {
		item.Reason = *row.ReasonRedacted
	}
	if row.ReplayOfRunID != nil {
		item.ReplayOfRunID = row.ReplayOfRunID.String()
	}
	for _, replayID := range row.ReplayedAsRunIDs {
		item.ReplayedAsRunIDs = append(item.ReplayedAsRunIDs, replayID.String())
	}
	return item
}
