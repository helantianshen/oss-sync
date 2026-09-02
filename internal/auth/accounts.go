// 账户管理
package auth

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/deviceauth"
	"github.com/oss/oss-server/internal/jwt"
	"github.com/oss/oss-server/internal/models"
)

var registrationMu sync.Mutex

// ValidateAccountInput 对 API 和网页注册使用同一套账号规则。
func ValidateAccountInput(username, password string) error {
	usernameLength := utf8.RuneCountInString(strings.TrimSpace(username))
	passwordLength := utf8.RuneCountInString(password)
	if usernameLength < 3 || usernameLength > 64 {
		return errors.New("用户名需要 3-64 个字符")
	}
	if passwordLength < 8 {
		return errors.New("密码至少需要 8 个字符")
	}
	// bcrypt 只接受最多 72 字节；明确拒绝，避免长密码被误报为用户名冲突。
	if len([]byte(password)) > 72 {
		return errors.New("密码的 UTF-8 编码不能超过 72 字节")
	}
	return nil
}

// IsUsernameTakenError 判断是否为用户名唯一约束冲突。
func IsUsernameTakenError(err error) bool {
	return errors.Is(err, gorm.ErrDuplicatedKey)
}

// CreateAccount 创建用户及默认用户设置。Vault 必须由用户登录后手动创建。
func CreateAccount(db *gorm.DB, username, password, role string) (*models.User, error) {
	username = strings.TrimSpace(username)
	role = strings.ToLower(strings.TrimSpace(role))
	if err := ValidateAccountInput(username, password); err != nil {
		return nil, err
	}
	if role != "admin" && role != "user" {
		return nil, errors.New("role 必须为 admin 或 user")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("生成密码哈希失败: %w", err)
	}
	user := models.User{
		Username:     username,
		PasswordHash: string(hash),
		Role:         role,
		StorageQuota: 0,
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		if err := tx.Create(&models.UserSetting{UserID: user.ID}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// CreateAccountForAnonymousRegistration 为匿名注册提供原子化的角色判定与创建。
// 角色判定与账户创建在同一进程锁 + 数据库事务内完成，避免并发首注产生多个 admin。
func CreateAccountForAnonymousRegistration(db *gorm.DB, username, password string) (*models.User, error) {
	if err := ValidateAccountInput(username, password); err != nil {
		return nil, err
	}
	username = strings.TrimSpace(username)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("生成密码哈希失败: %w", err)
	}
	registrationMu.Lock()
	defer registrationMu.Unlock()
	var user *models.User
	err = db.Transaction(func(tx *gorm.DB) error {
		var adminCount int64
		if err := tx.Model(&models.User{}).Where("role = ?", "admin").Count(&adminCount).Error; err != nil {
			return err
		}
		role := "user"
		if adminCount == 0 {
			role = "admin"
		}
		u := models.User{
			Username:     username,
			PasswordHash: string(hash),
			Role:         role,
			StorageQuota: 0,
		}
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		if err := tx.Create(&models.UserSetting{UserID: u.ID}).Error; err != nil {
			return err
		}
		user = &u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// AuthenticateCredentials 校验用户名密码，并返回当前数据库中的用户。
func AuthenticateCredentials(db *gorm.DB, username, password string) (*models.User, error) {
	var user models.User
	if err := db.Where("username = ?", strings.TrimSpace(username)).First(&user).Error; err != nil {
		return nil, errBadCred
	}
	if user.PasswordHash == "" ||
		bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		return nil, errBadCred
	}
	return &user, nil
}

// AuthenticateToken 校验网页管理面板 cookie 中保存的 JWT。
func AuthenticateToken(db *gorm.DB, cfg *config.Config, token string) (*models.User, error) {
	return authenticateBearer(db, cfg, token)
}

// AuthenticateIdentityToken validates a bearer token and returns its optional device binding.
func AuthenticateIdentityToken(db *gorm.DB, cfg *config.Config, token string) (*Identity, error) {
	return authenticateBearerIdentity(db, cfg, token)
}

// IssueToken 为用户签发与 API 登录相同的 JWT。
func IssueToken(cfg *config.Config, user models.User) (string, int64, error) {
	return issueToken(cfg, jwt.Claims{
		UserID:       user.ID,
		Username:     user.Username,
		Role:         user.Role,
		TokenVersion: user.TokenVersion,
	}, time.Duration(cfg.Auth.JWTTTLHours)*time.Hour)
}

// IssueWebToken 为网页管理端签发短期会话 JWT。
func IssueWebToken(cfg *config.Config, user models.User) (string, int64, error) {
	return issueToken(cfg, jwt.Claims{
		UserID:       user.ID,
		Username:     user.Username,
		Role:         user.Role,
		TokenVersion: user.TokenVersion,
	}, time.Duration(cfg.Auth.EffectiveWebSessionTTLHours())*time.Hour)
}

// IssueDeviceToken 为指定设备签发绑定 did 的 JWT，复用与 IssueToken 相同的签名逻辑。
func IssueDeviceToken(cfg *config.Config, user models.User, deviceID jwt.DeviceID) (string, int64, error) {
	normalized := deviceauth.NormalizeClientID(string(deviceID))
	if normalized == "" {
		return "", 0, errors.New("invalid device id")
	}
	return issueToken(cfg, jwt.Claims{
		UserID:       user.ID,
		Username:     user.Username,
		Role:         user.Role,
		TokenVersion: user.TokenVersion,
		DeviceID:     jwt.DeviceID(normalized),
	}, time.Duration(cfg.Auth.EffectiveDeviceJWTTTLHours())*time.Hour)
}

func issueToken(cfg *config.Config, claims jwt.Claims, ttl time.Duration) (string, int64, error) {
	token, err := jwt.Sign(cfg.Auth.JWTSecret, claims, ttl)
	if err != nil {
		return "", 0, err
	}
	return token, int64(ttl / time.Second), nil
}

// ChangePassword 校验旧密码后更新用户密码并递增 token 版本。
// 所有旧 JWT 在版本递增后立即失效。
func ChangePassword(db *gorm.DB, userID uint, oldPassword, newPassword string) error {
	var user models.User
	if err := db.First(&user, userID).Error; err != nil {
		return err
	}
	if user.PasswordHash == "" ||
		bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(oldPassword)) != nil {
		return errBadCred
	}
	if err := ValidateAccountInput(user.Username, newPassword); err != nil {
		return err
	}
	return updatePassword(db, userID, newPassword)
}

// SetPassword 由管理员调用，重置目标用户密码并递增 token 版本。
func SetPassword(db *gorm.DB, userID uint, newPassword string) error {
	var user models.User
	if err := db.First(&user, userID).Error; err != nil {
		return err
	}
	if err := ValidateAccountInput(user.Username, newPassword); err != nil {
		return err
	}
	return updatePassword(db, userID, newPassword)
}

func updatePassword(db *gorm.DB, userID uint, newPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("生成密码哈希失败: %w", err)
	}
	return db.Model(&models.User{}).Where("id = ?", userID).Updates(map[string]any{
		"password_hash": string(hash),
		"token_version": gorm.Expr("token_version + 1"),
	}).Error
}
