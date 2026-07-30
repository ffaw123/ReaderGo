package db

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Open 创建 API 和 Worker 共用的 PostgreSQL 数据库连接。
func Open(dsn string) (*gorm.DB, error) { return gorm.Open(postgres.Open(dsn), &gorm.Config{}) }
