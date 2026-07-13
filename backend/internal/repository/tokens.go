package repository

import (
	"context"
	"github.com/google/uuid"
)

func (s *Store) SaveAccessTokenHash(ctx context.Context, name, kind, hash string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM access_tokens WHERE name=$1 AND kind=$2`, name, kind); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO access_tokens(id,name,token_hash,kind) VALUES($1,$2,$3,$4)`, uuid.New(), name, hash, kind); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
