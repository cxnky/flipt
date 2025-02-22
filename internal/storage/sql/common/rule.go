package common

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	sq "github.com/Masterminds/squirrel"
	"github.com/gofrs/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	errs "go.flipt.io/flipt/errors"
	"go.flipt.io/flipt/internal/storage"
	fliptsql "go.flipt.io/flipt/internal/storage/sql"
	flipt "go.flipt.io/flipt/rpc/flipt"
)

// GetRule gets an individual rule
func (s *Store) GetRule(ctx context.Context, id string) (*flipt.Rule, error) {
	var (
		createdAt fliptsql.Timestamp
		updatedAt fliptsql.Timestamp

		rule = &flipt.Rule{}

		err = s.builder.Select("id, flag_key, segment_key, \"rank\", created_at, updated_at").
			From("rules").
			Where(sq.And{sq.Eq{"id": id}}).
			QueryRowContext(ctx).
			Scan(&rule.Id, &rule.FlagKey, &rule.SegmentKey, &rule.Rank, &createdAt, &updatedAt)
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errs.ErrNotFoundf("rule %q", id)
		}

		return nil, err
	}

	rule.CreatedAt = createdAt.Timestamp
	rule.UpdatedAt = updatedAt.Timestamp

	if err := s.distributions(ctx, rule); err != nil {
		return nil, err
	}

	return rule, nil
}

// ListRules gets all rules for a flag
func (s *Store) ListRules(ctx context.Context, flagKey string, opts ...storage.QueryOption) (storage.ResultSet[*flipt.Rule], error) {
	params := &storage.QueryParams{}

	for _, opt := range opts {
		opt(params)
	}

	var (
		rules   []*flipt.Rule
		results = storage.ResultSet[*flipt.Rule]{}

		query = s.builder.Select("id, flag_key, segment_key, \"rank\", created_at, updated_at").
			From("rules").
			Where(sq.Eq{"flag_key": flagKey}).
			OrderBy(fmt.Sprintf("\"rank\" %s", params.Order))
	)

	if params.Limit > 0 {
		query = query.Limit(params.Limit + 1)
	}

	var offset uint64
	if params.PageToken != "" {
		var token PageToken
		if err := json.Unmarshal([]byte(params.PageToken), &token); err != nil {
			return results, fmt.Errorf("decoding page token %w", err)
		}

		offset = token.Offset
		query = query.Offset(offset)
	} else if params.Offset > 0 {
		offset = params.Offset
		query = query.Offset(offset)
	}

	rows, err := query.QueryContext(ctx)
	if err != nil {
		return results, err
	}

	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	for rows.Next() {
		var (
			rule      flipt.Rule
			createdAt fliptsql.Timestamp
			updatedAt fliptsql.Timestamp
		)

		if err := rows.Scan(
			&rule.Id,
			&rule.FlagKey,
			&rule.SegmentKey,
			&rule.Rank,
			&createdAt,
			&updatedAt); err != nil {
			return results, err
		}

		rule.CreatedAt = createdAt.Timestamp
		rule.UpdatedAt = updatedAt.Timestamp

		if err := s.distributions(ctx, &rule); err != nil {
			return results, err
		}

		rules = append(rules, &rule)
	}

	if err := rows.Err(); err != nil {
		return results, err
	}

	var next *flipt.Rule

	if len(rules) > int(params.Limit) && params.Limit > 0 {
		next = rules[len(rules)-1]
		rules = rules[:params.Limit]
	}

	results.Results = rules

	if next != nil {
		out, err := json.Marshal(PageToken{Key: next.Id, Offset: offset + uint64(len(rules))})
		if err != nil {
			return results, fmt.Errorf("encoding page token %w", err)
		}
		results.NextPageToken = string(out)
	}

	return results, rows.Err()
}

// CountRules counts all rules
func (s *Store) CountRules(ctx context.Context) (uint64, error) {
	var count uint64

	if err := s.builder.Select("COUNT(*)").From("rules").QueryRowContext(ctx).Scan(&count); err != nil {
		return 0, err
	}

	return count, nil
}

// CreateRule creates a rule
func (s *Store) CreateRule(ctx context.Context, r *flipt.CreateRuleRequest) (*flipt.Rule, error) {
	var (
		now  = timestamppb.Now()
		rule = &flipt.Rule{
			Id:         uuid.Must(uuid.NewV4()).String(),
			FlagKey:    r.FlagKey,
			SegmentKey: r.SegmentKey,
			Rank:       r.Rank,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
	)

	if _, err := s.builder.
		Insert("rules").
		Columns("id", "flag_key", "segment_key", "\"rank\"", "created_at", "updated_at").
		Values(
			rule.Id,
			rule.FlagKey,
			rule.SegmentKey,
			rule.Rank,
			&fliptsql.Timestamp{Timestamp: rule.CreatedAt},
			&fliptsql.Timestamp{Timestamp: rule.UpdatedAt}).
		ExecContext(ctx); err != nil {
		return nil, err
	}

	return rule, nil
}

// UpdateRule updates an existing rule
func (s *Store) UpdateRule(ctx context.Context, r *flipt.UpdateRuleRequest) (*flipt.Rule, error) {
	query := s.builder.Update("rules").
		Set("segment_key", r.SegmentKey).
		Set("updated_at", &fliptsql.Timestamp{Timestamp: timestamppb.Now()}).
		Where(sq.And{sq.Eq{"id": r.Id}, sq.Eq{"flag_key": r.FlagKey}})

	res, err := query.ExecContext(ctx)
	if err != nil {
		return nil, err
	}

	count, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}

	if count != 1 {
		return nil, errs.ErrNotFoundf("rule %q", r.Id)
	}

	return s.GetRule(ctx, r.Id)
}

// DeleteRule deletes a rule
func (s *Store) DeleteRule(ctx context.Context, r *flipt.DeleteRuleRequest) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}

	// delete rule
	//nolint
	_, err = s.builder.Delete("rules").
		RunWith(tx).
		Where(sq.And{sq.Eq{"id": r.Id}, sq.Eq{"flag_key": r.FlagKey}}).
		ExecContext(ctx)

	// reorder existing rules after deletion
	rows, err := s.builder.Select("id").
		RunWith(tx).
		From("rules").
		Where(sq.Eq{"flag_key": r.FlagKey}).
		OrderBy("\"rank\" ASC").
		QueryContext(ctx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			_ = tx.Rollback()
			err = cerr
		}
	}()

	var ruleIDs []string

	for rows.Next() {
		var ruleID string

		if err := rows.Scan(&ruleID); err != nil {
			_ = tx.Rollback()
			return err
		}

		ruleIDs = append(ruleIDs, ruleID)
	}

	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := s.orderRules(ctx, tx, r.FlagKey, ruleIDs); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

// OrderRules orders rules
func (s *Store) OrderRules(ctx context.Context, r *flipt.OrderRulesRequest) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}

	if err := s.orderRules(ctx, tx, r.FlagKey, r.RuleIds); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

func (s *Store) orderRules(ctx context.Context, runner sq.BaseRunner, flagKey string, ruleIDs []string) error {
	updatedAt := timestamppb.Now()

	for i, id := range ruleIDs {
		_, err := s.builder.Update("rules").
			RunWith(runner).
			Set("\"rank\"", i+1).
			Set("updated_at", &fliptsql.Timestamp{Timestamp: updatedAt}).
			Where(sq.And{sq.Eq{"id": id}, sq.Eq{"flag_key": flagKey}}).
			ExecContext(ctx)
		if err != nil {
			return err
		}
	}

	return nil
}

// CreateDistribution creates a distribution
func (s *Store) CreateDistribution(ctx context.Context, r *flipt.CreateDistributionRequest) (*flipt.Distribution, error) {
	var (
		now = timestamppb.Now()
		d   = &flipt.Distribution{
			Id:        uuid.Must(uuid.NewV4()).String(),
			RuleId:    r.RuleId,
			VariantId: r.VariantId,
			Rollout:   r.Rollout,
			CreatedAt: now,
			UpdatedAt: now,
		}
	)

	if _, err := s.builder.
		Insert("distributions").
		Columns("id", "rule_id", "variant_id", "rollout", "created_at", "updated_at").
		Values(
			d.Id,
			d.RuleId,
			d.VariantId,
			d.Rollout,
			&fliptsql.Timestamp{Timestamp: d.CreatedAt},
			&fliptsql.Timestamp{Timestamp: d.UpdatedAt}).
		ExecContext(ctx); err != nil {
		return nil, err
	}

	return d, nil
}

// UpdateDistribution updates an existing distribution
func (s *Store) UpdateDistribution(ctx context.Context, r *flipt.UpdateDistributionRequest) (*flipt.Distribution, error) {
	query := s.builder.Update("distributions").
		Set("rollout", r.Rollout).
		Set("updated_at", &fliptsql.Timestamp{Timestamp: timestamppb.Now()}).
		Where(sq.And{sq.Eq{"id": r.Id}, sq.Eq{"rule_id": r.RuleId}, sq.Eq{"variant_id": r.VariantId}})

	res, err := query.ExecContext(ctx)
	if err != nil {
		return nil, err
	}

	count, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}

	if count != 1 {
		return nil, errs.ErrNotFoundf("distribution %q", r.Id)
	}

	var (
		createdAt fliptsql.Timestamp
		updatedAt fliptsql.Timestamp

		distribution = &flipt.Distribution{}
	)

	if err := s.builder.Select("id, rule_id, variant_id, rollout, created_at, updated_at").
		From("distributions").
		Where(sq.And{sq.Eq{"id": r.Id}, sq.Eq{"rule_id": r.RuleId}, sq.Eq{"variant_id": r.VariantId}}).
		QueryRowContext(ctx).
		Scan(&distribution.Id, &distribution.RuleId, &distribution.VariantId, &distribution.Rollout, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	distribution.CreatedAt = createdAt.Timestamp
	distribution.UpdatedAt = updatedAt.Timestamp

	return distribution, nil
}

// DeleteDistribution deletes a distribution
func (s *Store) DeleteDistribution(ctx context.Context, r *flipt.DeleteDistributionRequest) error {
	_, err := s.builder.Delete("distributions").
		Where(sq.And{sq.Eq{"id": r.Id}, sq.Eq{"rule_id": r.RuleId}, sq.Eq{"variant_id": r.VariantId}}).
		ExecContext(ctx)

	return err
}

func (s *Store) distributions(ctx context.Context, rule *flipt.Rule) (err error) {
	query := s.builder.Select("id", "rule_id", "variant_id", "rollout", "created_at", "updated_at").
		From("distributions").
		Where(sq.Eq{"rule_id": rule.Id}).
		OrderBy("created_at ASC")

	rows, err := query.QueryContext(ctx)
	if err != nil {
		return err
	}

	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	for rows.Next() {
		var (
			distribution         flipt.Distribution
			createdAt, updatedAt fliptsql.Timestamp
		)

		if err := rows.Scan(
			&distribution.Id,
			&distribution.RuleId,
			&distribution.VariantId,
			&distribution.Rollout,
			&createdAt,
			&updatedAt); err != nil {
			return err
		}

		distribution.CreatedAt = createdAt.Timestamp
		distribution.UpdatedAt = updatedAt.Timestamp

		rule.Distributions = append(rule.Distributions, &distribution)
	}

	return rows.Err()
}
