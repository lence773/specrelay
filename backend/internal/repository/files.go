package repository

import (
	"context"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

func (s *Store) CreateAttachment(ctx context.Context, a domain.Attachment) (domain.Attachment, error) {
	err := s.Pool.QueryRow(ctx, `INSERT INTO attachments(id,project_id,intake_id,original_name,mime_type,size_bytes,sha256,storage_path) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at,updated_at,version`, a.ID, a.ProjectID, a.IntakeID, a.OriginalName, a.MimeType, a.SizeBytes, a.SHA256, a.StoragePath).Scan(&a.CreatedAt, &a.UpdatedAt, &a.Version)
	return a, err
}
func (s *Store) ListAttachments(ctx context.Context, intakeID any) ([]domain.Attachment, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,project_id,intake_id,original_name,mime_type,size_bytes,sha256,storage_path,created_at,updated_at,version FROM attachments WHERE intake_id=$1 ORDER BY created_at`, intakeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Attachment{}
	for rows.Next() {
		var a domain.Attachment
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.IntakeID, &a.OriginalName, &a.MimeType, &a.SizeBytes, &a.SHA256, &a.StoragePath, &a.CreatedAt, &a.UpdatedAt, &a.Version); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
