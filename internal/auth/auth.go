package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// Service 持有 JWT 签发配置；签名密钥不对外暴露。
type Service struct {
	secret []byte
	ttl    time.Duration
}

// Claims 包含已认证的租户 ID 及标准过期时间等元数据。
type Claims struct {
	UserID string `json:"uid"`
	jwt.RegisteredClaims
}

// New 使用配置的访问令牌有效期创建 HS256 JWT 服务。
func New(secret string, hours int) *Service {
	return &Service{secret: []byte(secret), ttl: time.Duration(hours) * time.Hour}
}

// HashPassword 使用 bcrypt 哈希密码，绝不持久化明文凭据。
func HashPassword(p string) (string, error) {
	b, e := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(b), e
}

// CheckPassword 使用 bcrypt 校验登录密码，避免直接比较明文。
func CheckPassword(hash, p string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(p))
}

// Token 签发短期 Bearer Token，并将用户 ID 作为后续请求的租户上下文。
func (s *Service) Token(userID string) (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{UserID: userID, RegisteredClaims: jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl))}}).SignedString(s.secret)
}

// Middleware 拒绝未认证请求，并把已验证的用户 ID 写入 Gin 上下文。
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			unauthorized(c)
			return
		}
		claims := &Claims{}
		// 显式限定 HS256，防止服务接受使用其他算法伪造的 Token。
		t, e := jwt.ParseWithClaims(strings.TrimPrefix(h, "Bearer "), claims, func(t *jwt.Token) (interface{}, error) {
			if t.Method != jwt.SigningMethodHS256 {
				return nil, errors.New("unexpected signing method")
			}
			return s.secret, nil
		})
		if e != nil || !t.Valid || claims.UserID == "" {
			unauthorized(c)
			return
		}
		c.Set("userID", claims.UserID)
		c.Next()
	}
}

// UserID 读取 Middleware 写入的身份；受保护路由调用前已完成鉴权。
func UserID(c *gin.Context) string { v, _ := c.Get("userID"); s, _ := v.(string); return s }

// unauthorized 中断后续处理链，确保受保护接口不会继续执行。
func unauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing access token"})
}
