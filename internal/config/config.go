package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config 保存 API 与 Worker 共用的运行时配置。
// 密钥只从环境变量读取，避免写入源代码。
type Config struct {
	HTTPAddr, DatabaseURL, KafkaBrokers, JWTSecret string
	JWTTTLHours                                    int
}

// Load 优先加载本地 .env，再校验生产环境必填配置。
func Load() (Config, error) {
	_ = godotenv.Load()
	c := Config{HTTPAddr: value("HTTP_ADDR", ":8080"), DatabaseURL: os.Getenv("DATABASE_URL"), KafkaBrokers: value("KAFKA_BROKERS", "localhost:9092"), JWTSecret: os.Getenv("JWT_SECRET"), JWTTTLHours: intValue("JWT_TTL_HOURS", 24)}
	if c.DatabaseURL == "" || len(c.JWTSecret) < 32 {
		return Config{}, fmt.Errorf("DATABASE_URL and a JWT_SECRET of at least 32 characters are required")
	}
	return c, nil
}

// Brokers 将逗号分隔的环境变量转换为 Sarama 所需的 Broker 地址列表。
func (c Config) Brokers() []string { return strings.Split(c.KafkaBrokers, ",") }

// value 仅在环境变量未设置时返回默认值。
func value(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

// intValue 在可选数字配置格式错误时回退默认值，避免服务无法启动。
func intValue(k string, fallback int) int {
	v, e := strconv.Atoi(value(k, strconv.Itoa(fallback)))
	if e != nil {
		return fallback
	}
	return v
}
