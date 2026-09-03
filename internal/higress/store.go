// Package higress implements the real Higress AI Gateway consumer-resolver
// service. It replaces the in-memory Python mock at bin/mock_higress.py.
//
// Storage: SQLite via gorm. Consumers are registered through the admin API
// (or the bootstrap sub-command) and persisted across restarts. The apikey
// is never stored in the clear — we keep a SHA-256 fingerprint plus (for
// AgentTeams workers that ship an HS256 JWT) the JWT claim payload needed
// for role inference.
//
// Resolution is two-step:
//  1. compute apikey fingerprint, look up consumer
//  2. if consumer requires JWT, verify signature with the configured secret
//
// This is the same crypto opskeeper uses to issue its AgentTeams tokens, so
// the consumer store treats an opskeeper-issued JWT as a credential of the
// form "<its-hash>" rather than a "secret everyone knows".
package higress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Consumer is the persisted row for a Higress-style consumer.
//
// Name must be opskeeper-<role> for AgentTeams workers so that
// opskeeper's inferRoleFromConsumerName() strips the prefix correctly.
type Consumer struct {
	Name         string    `gorm:"column:name;primaryKey;size:128"`
	ApikeyHash   string    `gorm:"column:apikey_hash;size:128;index"`
	JWTRequired  bool      `gorm:"column:jwt_required"`
	WorkerClaim  string    `gorm:"column:worker_claim;size:128"`
	RoleClaim    string    `gorm:"column:role_claim;size:64"`
	TenantClaim  string    `gorm:"column:tenant_claim;size:128"`
	MetadataJSON string    `gorm:"column:metadata;type:text"`
	CreatedAt    time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt    time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName overrides gorm's default pluralized naming.
func (Consumer) TableName() string { return "consumers" }

// ErrConsumerNotFound is returned when no consumer matches the apikey.
var ErrConsumerNotFound = errors.New("higress: consumer not found")

// ErrConsumerExists is returned by Create when the name is already taken.
var ErrConsumerExists = errors.New("higress: consumer already exists")

// Store is the persistent consumer store.
type Store struct {
	db   *gorm.DB
	lock sync.RWMutex
}

// NewStore opens (or creates) the consumer database at the given path.
func NewStore(path string) (*Store, error) {
	gormLogger := logger.New(
		newDiscardWriter(),
		logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Error,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)
	db, err := gorm.Open(sqliteDialector(path), &gorm.Config{
		Logger: gormLogger,
	})
	if err != nil {
		return nil, fmt.Errorf("open higress store: %w", err)
	}
	if err := db.AutoMigrate(&Consumer{}); err != nil {
		return nil, fmt.Errorf("migrate higress store: %w", err)
	}
	return &Store{db: db}, nil
}

// Fingerprint returns the SHA-256 hex digest used as apikey hash.
func Fingerprint(apikey string) string {
	sum := sha256.Sum256([]byte(apikey))
	return hex.EncodeToString(sum[:])
}

// Create registers a new consumer. apikey is hashed before persistence.
func (s *Store) Create(ctx context.Context, c Consumer, apikey string) error {
	if c.Name == "" {
		return errors.New("higress: consumer name is required")
	}
	if apikey == "" {
		return errors.New("higress: apikey is required")
	}
	c.ApikeyHash = Fingerprint(apikey)
	s.lock.Lock()
	defer s.lock.Unlock()
	var existing Consumer
	err := s.db.WithContext(ctx).Where("name = ?", c.Name).First(&existing).Error
	if err == nil {
		return ErrConsumerExists
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return s.db.WithContext(ctx).Create(&c).Error
}

// Upsert registers a consumer or updates an existing one (used by bootstrap).
func (s *Store) Upsert(ctx context.Context, c Consumer, apikey string) error {
	if c.Name == "" {
		return errors.New("higress: consumer name is required")
	}
	if apikey == "" {
		return errors.New("higress: apikey is required")
	}
	c.ApikeyHash = Fingerprint(apikey)
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.db.WithContext(ctx).
		Clauses(gormOnConflictName).
		Save(&c).Error
}

// Resolve looks up a consumer by apikey fingerprint.
func (s *Store) Resolve(ctx context.Context, apikey string) (Consumer, error) {
	if apikey == "" {
		return Consumer{}, ErrConsumerNotFound
	}
	hash := Fingerprint(apikey)
	s.lock.RLock()
	defer s.lock.RUnlock()
	var c Consumer
	err := s.db.WithContext(ctx).Where("apikey_hash = ?", hash).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Consumer{}, ErrConsumerNotFound
	}
	return c, err
}

// List returns all consumers (no apikey material).
func (s *Store) List(ctx context.Context) ([]Consumer, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var rows []Consumer
	err := s.db.WithContext(ctx).Order("name ASC").Find(&rows).Error
	return rows, err
}

// Get returns one consumer by name (no apikey material).
func (s *Store) Get(ctx context.Context, name string) (Consumer, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var c Consumer
	err := s.db.WithContext(ctx).Where("name = ?", name).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Consumer{}, ErrConsumerNotFound
	}
	return c, err
}

// Delete removes a consumer by name.
func (s *Store) Delete(ctx context.Context, name string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	res := s.db.WithContext(ctx).Where("name = ?", name).Delete(&Consumer{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrConsumerNotFound
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
