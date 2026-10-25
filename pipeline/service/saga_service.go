package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"mercatus/pipeline/model"
	"mercatus/pipeline/repository"
)

type Step struct {
	Name string
	Do   func(ctx context.Context) error
	Undo func(ctx context.Context) error
}

type SagaService struct {
	sagas *repository.SagaRepository
}

func NewSagaService(s *repository.SagaRepository) *SagaService { return &SagaService{sagas: s} }

var ErrStepFailed = errors.New("saga step failed")

func (s *SagaService) Run(ctx context.Context, saga *model.Saga, steps []Step) error {
	var completed []Step
	for _, step := range steps {
		if err := s.sagas.SetStep(ctx, saga.ID, step.Name); err != nil {
			_ = s.sagas.SetStatus(ctx, saga.ID, model.SagaCompensating)
			s.compensate(ctx, saga.ID, completed)
			_ = s.sagas.SetStatus(ctx, saga.ID, model.SagaFailed)
			saga.Status = model.SagaFailed
			return fmt.Errorf("%w at %s: %w", ErrStepFailed, step.Name, err)
		}
		if err := step.Do(ctx); err != nil {
			_ = s.sagas.RecordStep(ctx, saga.ID, step.Name, "failed")
			_ = s.sagas.SetStatus(ctx, saga.ID, model.SagaCompensating)
			_ = s.sagas.SetError(ctx, saga.ID, err.Error())
			s.compensate(ctx, saga.ID, completed)
			_ = s.sagas.SetStatus(ctx, saga.ID, model.SagaFailed)
			saga.Status = model.SagaFailed
			return fmt.Errorf("%w at %s: %w", ErrStepFailed, step.Name, err)
		}
		if err := s.sagas.RecordStep(ctx, saga.ID, step.Name, "done"); err != nil {
			slog.ErrorContext(ctx, "could not record saga step", "saga_id", saga.ID, "step", step.Name, "error", err)
		}
		completed = append(completed, step)
	}
	if err := s.sagas.SetStatus(ctx, saga.ID, model.SagaCompleted); err != nil {
		return err
	}
	saga.Status = model.SagaCompleted
	return nil
}

func (s *SagaService) compensate(ctx context.Context, sagaID int64, completed []Step) {
	for i := len(completed) - 1; i >= 0; i-- {
		step := completed[i]
		if step.Undo != nil {
			if err := retry(ctx, 3, step.Undo); err != nil {
				slog.ErrorContext(ctx, "saga compensation failed", "saga_id", sagaID, "step", step.Name, "error", err)
				continue
			}
		}
		_ = s.sagas.MarkStepCompensated(ctx, sagaID, step.Name)
	}
}

func (s *SagaService) Get(ctx context.Context, id int64) (*model.Saga, []*model.SagaStep, error) {
	saga, err := s.sagas.Get(ctx, id)
	if err != nil || saga == nil {
		return nil, nil, err
	}
	steps, err := s.sagas.Steps(ctx, id)
	return saga, steps, err
}

func (s *SagaService) NewSaga(ctx context.Context, orderID int64) (*model.Saga, error) {
	saga := &model.Saga{OrderID: orderID}
	if err := s.sagas.Create(ctx, saga); err != nil {
		return nil, err
	}
	return saga, nil
}
