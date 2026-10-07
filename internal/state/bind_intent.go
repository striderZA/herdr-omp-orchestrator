package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GetBindIntent reads an unresolved primary workspace-creation intent.
func (s *Store) GetBindIntent(ctx context.Context, key string) (BindIntent, error) {
	if strings.TrimSpace(key) == "" {
		return BindIntent{}, fmt.Errorf("%w: bind-intent key is required", ErrInvalid)
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM bind_intents WHERE key = ?`, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return BindIntent{}, fmt.Errorf("%w: bind intent for %q", ErrNotFound, key)
	}
	if err != nil {
		return BindIntent{}, fmt.Errorf("read bind intent for %q: %w", key, err)
	}
	return decodeBindIntent(key, data)
}

// ClaimBindIntent creates or leases one bind intent. A live lease is never
// stolen; an expired create is marked uncertain before another caller may inspect it.
func (s *Store) ClaimBindIntent(
	ctx context.Context,
	key, owner string,
	lease time.Duration,
	paneIDs, workspaceIDs []string,
) (BindIntent, bool, bool, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(owner) == "" || lease.Milliseconds() <= 0 {
		return BindIntent{}, false, false, fmt.Errorf("%w: bind-intent key, owner and positive lease are required", ErrInvalid)
	}
	if err := validateBindIntentIDs("pane", paneIDs); err != nil {
		return BindIntent{}, false, false, err
	}
	if err := validateBindIntentIDs("workspace", workspaceIDs); err != nil {
		return BindIntent{}, false, false, err
	}
	tx, err := s.begin(ctx, "claim bind intent for "+key)
	if err != nil {
		return BindIntent{}, false, false, err
	}
	defer tx.rollback()

	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM bind_intents WHERE key = ?`, key).Scan(&data)
	now := time.Now().UnixMilli()
	until := now + lease.Milliseconds()
	if errors.Is(err, sql.ErrNoRows) {
		intent := BindIntent{
			Key: key, PaneIDs: append([]string(nil), paneIDs...), WorkspaceIDs: append([]string(nil), workspaceIDs...),
			Owner: owner, LeaseUntil: until, Phase: BindIntentPrepared,
		}
		encoded, err := json.Marshal(intent)
		if err != nil {
			return BindIntent{}, false, false, fmt.Errorf("encode bind intent for %q: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO bind_intents (key, data) VALUES (?, ?)`, key, string(encoded)); err != nil {
			return BindIntent{}, false, false, fmt.Errorf("create bind intent for %q: %w", key, err)
		}
		if err := tx.commit("claim bind intent for " + key); err != nil {
			return BindIntent{}, false, false, err
		}
		return intent, true, true, nil
	}
	if err != nil {
		return BindIntent{}, false, false, fmt.Errorf("read bind intent for %q while claiming: %w", key, err)
	}
	intent, err := decodeBindIntent(key, data)
	if err != nil {
		return BindIntent{}, false, false, err
	}
	if intent.Owner != "" && now < intent.LeaseUntil {
		if err := tx.commit("observe live bind intent for " + key); err != nil {
			return BindIntent{}, false, false, err
		}
		return intent, false, false, nil
	}
	if intent.Phase == BindIntentCreating {
		// The former owner stopped while the external mutation was in flight.
		// Expiry makes the intent inspectable, not repeatable.
		intent.Phase = BindIntentUncertain
	}
	intent.Owner = owner
	intent.LeaseUntil = until
	if err := saveBindIntent(ctx, tx, intent); err != nil {
		return BindIntent{}, false, false, err
	}
	if err := tx.commit("reclaim bind intent for " + key); err != nil {
		return BindIntent{}, false, false, err
	}
	return intent, true, false, nil
}

// MarkBindIntentCreating durably records that the next step may mutate Herdr.
func (s *Store) MarkBindIntentCreating(ctx context.Context, key, owner string, lease time.Duration) error {
	if lease.Milliseconds() <= 0 {
		return fmt.Errorf("%w: bind-intent lease must be positive", ErrInvalid)
	}
	return s.mutateBindIntent(ctx, key, owner, "mark bind intent creating", func(intent *BindIntent) error {
		if intent.Phase != BindIntentPrepared {
			return fmt.Errorf("%w: bind intent for %q is %q, not prepared", ErrConflict, key, intent.Phase)
		}
		intent.Phase = BindIntentCreating
		intent.LeaseUntil = time.Now().UnixMilli() + lease.Milliseconds()
		return nil
	})
}

// RefreshBindIntentBaseline updates only a prepared intent. It cannot erase
// evidence after a workspace-create side effect may have started.
func (s *Store) RefreshBindIntentBaseline(
	ctx context.Context,
	key, owner string,
	lease time.Duration,
	paneIDs, workspaceIDs []string,
) error {
	if lease.Milliseconds() <= 0 {
		return fmt.Errorf("%w: bind-intent lease must be positive", ErrInvalid)
	}
	if err := validateBindIntentIDs("pane", paneIDs); err != nil {
		return err
	}
	if err := validateBindIntentIDs("workspace", workspaceIDs); err != nil {
		return err
	}
	return s.mutateBindIntent(ctx, key, owner, "refresh bind intent baseline", func(intent *BindIntent) error {
		if intent.Phase != BindIntentPrepared {
			return fmt.Errorf("%w: bind intent for %q is %q, not prepared", ErrConflict, key, intent.Phase)
		}
		intent.PaneIDs = append([]string(nil), paneIDs...)
		intent.WorkspaceIDs = append([]string(nil), workspaceIDs...)
		intent.LeaseUntil = time.Now().UnixMilli() + lease.Milliseconds()
		return nil
	})
}

// MarkBindIntentUncertain preserves an outcome that may have taken effect and
// releases the lease so a later bind can inspect it.
func (s *Store) MarkBindIntentUncertain(ctx context.Context, key, owner string) error {
	return s.mutateBindIntent(ctx, key, owner, "mark bind intent uncertain", func(intent *BindIntent) error {
		if intent.Phase != BindIntentCreating {
			return fmt.Errorf("%w: bind intent for %q is %q, not creating", ErrConflict, key, intent.Phase)
		}
		intent.Phase = BindIntentUncertain
		intent.Owner = ""
		intent.LeaseUntil = 0
		return nil
	})
}

// ReleaseBindIntent leaves its phase intact while allowing another caller to
// inspect or explicitly recover it.
func (s *Store) ReleaseBindIntent(ctx context.Context, key, owner string) error {
	return s.mutateBindIntent(ctx, key, owner, "release bind intent", func(intent *BindIntent) error {
		intent.Owner = ""
		intent.LeaseUntil = 0
		return nil
	})
}

// DeleteBindIntent completes an intent owned by this caller after the project row
// records the workspace and pane identities.
func (s *Store) DeleteBindIntent(ctx context.Context, key, owner string) error {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(owner) == "" {
		return fmt.Errorf("%w: bind-intent key and owner are required", ErrInvalid)
	}
	tx, err := s.begin(ctx, "complete bind intent for "+key)
	if err != nil {
		return err
	}
	defer tx.rollback()
	intent, err := readBindIntent(ctx, tx, key)
	if err != nil {
		return err
	}
	if intent.Owner != owner {
		return fmt.Errorf("%w: bind intent for %q is held by %q", ErrConflict, key, intent.Owner)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM bind_intents WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete bind intent for %q: %w", key, err)
	}
	return tx.commit("complete bind intent for " + key)
}

// ClearBindIntent removes an intent when the durable project row already proves
// that its workspace and pane identities were recorded.
func (s *Store) ClearBindIntent(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("%w: bind-intent key is required", ErrInvalid)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM bind_intents WHERE key = ?`, key); err != nil {
		return fmt.Errorf("clear bind intent for %q: %w", key, err)
	}
	return nil
}

func (s *Store) mutateBindIntent(
	ctx context.Context,
	key, owner, what string,
	mutate func(*BindIntent) error,
) error {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(owner) == "" {
		return fmt.Errorf("%w: bind-intent key and owner are required", ErrInvalid)
	}
	tx, err := s.begin(ctx, what+" for "+key)
	if err != nil {
		return err
	}
	defer tx.rollback()
	intent, err := readBindIntent(ctx, tx, key)
	if err != nil {
		return err
	}
	if intent.Owner != owner {
		return fmt.Errorf("%w: bind intent for %q is held by %q", ErrConflict, key, intent.Owner)
	}
	if err := mutate(&intent); err != nil {
		return err
	}
	if err := saveBindIntent(ctx, tx, intent); err != nil {
		return err
	}
	return tx.commit(what + " for " + key)
}

func readBindIntent(ctx context.Context, tx *tx, key string) (BindIntent, error) {
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM bind_intents WHERE key = ?`, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return BindIntent{}, fmt.Errorf("%w: bind intent for %q", ErrNotFound, key)
	}
	if err != nil {
		return BindIntent{}, fmt.Errorf("read bind intent for %q: %w", key, err)
	}
	return decodeBindIntent(key, data)
}

func decodeBindIntent(key string, data []byte) (BindIntent, error) {
	var intent BindIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return BindIntent{}, fmt.Errorf("decode bind intent for %q: %w", key, err)
	}
	if intent.Key != key {
		return BindIntent{}, fmt.Errorf("%w: bind intent key %q contradicts its row key %q", ErrInvalid, intent.Key, key)
	}
	switch intent.Phase {
	case BindIntentPrepared, BindIntentCreating, BindIntentUncertain:
		return intent, nil
	default:
		return BindIntent{}, fmt.Errorf("%w: bind intent for %q has unknown phase %q", ErrInvalid, key, intent.Phase)
	}
}

func saveBindIntent(ctx context.Context, tx *tx, intent BindIntent) error {
	encoded, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("encode bind intent for %q: %w", intent.Key, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE bind_intents SET data = ? WHERE key = ?`, string(encoded), intent.Key)
	if err != nil {
		return fmt.Errorf("update bind intent for %q: %w", intent.Key, err)
	}
	return requireOneRow(res, ErrNotFound, "update bind intent "+intent.Key)
}

func validateBindIntentIDs(kind string, ids []string) error {
	for i, id := range ids {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%w: bind-intent %s id %d is empty", ErrInvalid, kind, i)
		}
	}
	return nil
}
