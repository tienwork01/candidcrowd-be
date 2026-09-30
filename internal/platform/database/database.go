package database

import (
	"database/sql"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func Open(url string, maxOpen, maxIdle int, lifetime time.Duration) (*gorm.DB, *sql.DB, error) {
	// The production Neon URL uses its transaction pooler. Prepared statements
	// are connection-local, so both GORM's and pgx's statement caches must stay
	// off when a later request can receive a different PostgreSQL connection.
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  url,
		PreferSimpleProtocol: true,
	}), &gorm.Config{TranslateError: true})
	if err != nil {
		return nil, nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(lifetime)
	return db, sqlDB, nil
}
