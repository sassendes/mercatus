package service

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"mercatus/admin/model"
	"mercatus/admin/repository"
)

var (
	ErrInvalidTarget  = errors.New("invalid target type")
	ErrInvalidAction  = errors.New("invalid action for this target")
	ErrReasonRequired = errors.New("a reason between 3 and 500 characters is required")
	ErrNotFound       = errors.New("target not found or already in that state")
)

var validTargets = map[string]bool{"product": true, "seller": true, "review": true, "buyer": true}

type AdminService struct{ admin *repository.AdminRepository }

func NewAdminService(a *repository.AdminRepository) *AdminService { return &AdminService{admin: a} }

func (s *AdminService) Moderate(ctx context.Context, adminID int64, req *model.ModerateRequest) (*model.ModerationAction, error) {
	if !validTargets[req.TargetType] {
		return nil, ErrInvalidTarget
	}
	if !repository.Supported(req.TargetType, req.Action) {
		return nil, ErrInvalidAction
	}
	reason := strings.TrimSpace(req.Reason)
	if n := utf8.RuneCountInString(reason); n < 3 || n > 500 {
		return nil, ErrReasonRequired
	}
	a := &model.ModerationAction{
		AdminID: adminID, TargetType: req.TargetType, TargetID: req.TargetID, Action: req.Action, Reason: reason,
	}
	if err := s.admin.Apply(ctx, a); err != nil {
		if errors.Is(err, repository.ErrTargetNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return a, nil
}

func (s *AdminService) AuditLog(ctx context.Context) ([]*model.ModerationAction, error) {
	return s.admin.ListActions(ctx, 200)
}

func (s *AdminService) Stats(ctx context.Context) (*model.PlatformStats, error) {
	return s.admin.Stats(ctx)
}
