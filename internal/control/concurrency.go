package control

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/state"
)

// ConcurrencyView reports the slots an entity holds across the whole cluster
// and its effective limit. An unlimited entity (limit 0) is not counted, so
// its current value is 0 without a Redis read.
type ConcurrencyView struct {
	Current int64 `json:"current"`
	Limit   int64 `json:"limit"`
}

func normalizeConcurrencyLimit(field optionalField[int64]) (*int64, error) {
	if !field.Set || field.Null {
		return nil, nil
	}
	if field.Value < 0 || field.Value > maxSafeInteger {
		return nil, app_errors.ErrValidation
	}
	value := field.Value
	return &value, nil
}

func accessKeyConcurrencyLimit(snapshot *state.ConfigSnapshot, accessKeyID uint) int64 {
	if snapshot == nil {
		return 0
	}
	return snapshot.AccessKeysByID[accessKeyID].ConcurrencyLimit
}

// fillAccessKeyConcurrency reads one access key's held slots from the shared
// cluster store. Callers must not hold writeMu: the store performs network I/O.
func (s *Service) fillAccessKeyConcurrency(ctx context.Context, value *AccessKeyMetadata) error {
	value.Concurrency = ConcurrencyView{Limit: accessKeyConcurrencyLimit(s.manager.Current(), value.ID)}
	if value.Concurrency.Limit == 0 {
		return nil
	}
	counts, err := s.clusterConcurrency.AccessKeyInflight(ctx, []uint{value.ID})
	if err != nil {
		return concurrencyStateError("access key", err)
	}
	value.Concurrency.Current = counts[value.ID]
	return nil
}

// fillCommittedAccessKeyConcurrency serves responses of already committed
// mutations. A failed read only logs and reports zero held slots: returning an
// error would make clients retry into an idempotent replay, which never
// carries the one-time plaintext key again.
func (s *Service) fillCommittedAccessKeyConcurrency(ctx context.Context, value *AccessKeyMetadata) {
	if err := s.fillAccessKeyConcurrency(ctx, value); err != nil {
		logrus.WithError(err).WithField("access_key_id", value.ID).Warn("Access key concurrency unavailable after commit")
	}
}

// fillAccessKeyCollectionConcurrency reads every limited key's held slots in
// one round trip.
func (s *Service) fillAccessKeyCollectionConcurrency(ctx context.Context, snapshot *state.ConfigSnapshot, records []accessKeyCollectionRecord) error {
	limited := make([]uint, 0, len(records))
	for index := range records {
		records[index].Concurrency = ConcurrencyView{Limit: accessKeyConcurrencyLimit(snapshot, records[index].ID)}
		if records[index].Concurrency.Limit > 0 {
			limited = append(limited, records[index].ID)
		}
	}
	if len(limited) == 0 {
		return nil
	}
	counts, err := s.clusterConcurrency.AccessKeyInflight(ctx, limited)
	if err != nil {
		return concurrencyStateError("access key", err)
	}
	for index := range records {
		records[index].Concurrency.Current = counts[records[index].ID]
	}
	return nil
}

// fillGroupConcurrency reads the held slots of the limited groups among items
// in one round trip; Limit must already come from the snapshot.
func (s *Service) fillGroupConcurrency(ctx context.Context, items []*GroupCollectionItem) error {
	limited := make([]uint, 0, len(items))
	for _, item := range items {
		if item.Concurrency.Limit > 0 {
			limited = append(limited, item.ID)
		}
	}
	if len(limited) == 0 {
		return nil
	}
	counts, err := s.clusterConcurrency.GroupInflight(ctx, limited)
	if err != nil {
		return concurrencyStateError("group", err)
	}
	for _, item := range items {
		item.Concurrency.Current = counts[item.ID]
	}
	return nil
}

func (s *Service) globalConcurrency(ctx context.Context) (ConcurrencyView, error) {
	view := ConcurrencyView{}
	if snapshot := s.manager.Current(); snapshot != nil {
		view.Limit = snapshot.Settings.GlobalConcurrencyLimit
	}
	if view.Limit == 0 {
		return view, nil
	}
	current, err := s.clusterConcurrency.GlobalInflight(ctx)
	if err != nil {
		return ConcurrencyView{}, concurrencyStateError("global", err)
	}
	view.Current = current
	return view, nil
}

func concurrencyStateError(scope string, err error) error {
	return fmt.Errorf("read %s concurrency state: %v: %w", scope, err, app_errors.ErrInternalServer)
}
