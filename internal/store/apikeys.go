package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const apiKeyPrefix = "sockt_live_"

func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	q := `SELECT id, user_id, key_hash, key_prefix, name, is_active FROM public.api_keys WHERE key_hash = $1`
	var k APIKey
	err := s.Tx.QueryRow(ctx, q, keyHash).Scan(&k.ID, &k.UserID, &k.KeyHash, &k.KeyPrefix, &k.Name, &k.IsActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (s *Store) CreateAPIKey(ctx context.Context, userID uuid.UUID, name string) (rawKey string, k *APIKey, err error) {
	raw := apiKeyPrefix + randomKeySuffix(32)
	hash := HashAPIKey(raw)
	prefix := raw
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	id := uuid.New()
	_, err = s.Tx.Exec(ctx, `INSERT INTO public.api_keys (id, user_id, key_hash, key_prefix, name, is_active) VALUES ($1,$2,$3,$4,$5,true)`,
		id, userID, hash, prefix, name)
	if err != nil {
		return "", nil, err
	}
	return raw, &APIKey{ID: id, UserID: userID, KeyHash: hash, KeyPrefix: prefix, Name: name, IsActive: true}, nil
}

func randomKeySuffix(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func (s *Store) ListAPIKeysByUser(ctx context.Context, userID uuid.UUID) ([]APIKey, error) {
	rows, err := s.Tx.Query(ctx, `SELECT id, user_id, key_hash, key_prefix, name, is_active FROM public.api_keys WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.KeyHash, &k.KeyPrefix, &k.Name, &k.IsActive); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, userID, keyID uuid.UUID) error {
	tag, err := s.Tx.Exec(ctx, `UPDATE public.api_keys SET is_active = false WHERE id = $1 AND user_id = $2`, keyID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("api key not found")
	}
	_, err = s.Tx.Exec(ctx, `SELECT pg_notify('apikey_revoked', $1::text)`, keyID.String())
	return err
}

func (s *Store) TouchAPIKeyUsed(ctx context.Context, keyID uuid.UUID) {
	_, _ = s.Tx.Exec(ctx, `UPDATE public.api_keys SET last_used_at = now() WHERE id = $1`, keyID)
}
