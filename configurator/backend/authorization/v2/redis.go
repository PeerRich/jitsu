package v2

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/gomodule/redigo/redis"
	"github.com/jitsucom/jitsu/configurator/handlers"
	"github.com/jitsucom/jitsu/configurator/middleware"
	"github.com/jitsucom/jitsu/configurator/openapi"
	"github.com/jitsucom/jitsu/server/logging"
	"github.com/jitsucom/jitsu/server/meta"
	"github.com/jitsucom/jitsu/server/timestamp"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
)

var (
	errUnknownToken             = errors.New("unknown token")
	errExpiredToken             = errors.New("expired token")
	errMailServiceNotConfigured = errors.New("mail service not configured")
)

const (
	usersIndexKey                = "users_index"
	userIDField                  = "id"
	userEmailField               = "email"
	userHashedPasswordField      = "hashed_password"
	userForceChangePasswordField = "force_change_password"
	resetIDTTL                   = 3600
)

type RedisInit struct {
	PoolFactory *meta.RedisPoolFactory
	MailSender  MailSender
}

type Redis struct {
	serverToken     string
	passwordEncoder PasswordEncoder
	redisPool       *meta.RedisPool
	mailSender      MailSender
}

func NewRedis(init RedisInit) (*Redis, error) {
	if redisPool, err := init.PoolFactory.Create(); err != nil {
		return nil, errors.Wrap(err, "create redis pool")
	} else {
		return &Redis{
			passwordEncoder: _bcrypt{},
			redisPool:       redisPool,
			mailSender:      init.MailSender,
		}, nil
	}
}

func (r *Redis) AuthorizationType() string {
	return "redis"
}

func (r *Redis) Local() (handlers.LocalAuthorizator, error) {
	return r, nil
}

func (r *Redis) Cloud() (handlers.CloudAuthorizator, error) {
	return nil, handlers.ErrIsLocal
}

func (r *Redis) Close() error {
	return r.redisPool.Close()
}

func (r *Redis) Authorize(ctx context.Context, token string) (*middleware.Authority, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return nil, err
	}

	defer closeQuietly(conn)

	tokenType := accessTokenType
	if token, err := r.getToken(conn, tokenType, token); err != nil {
		return nil, errors.Wrap(err, "find token")
	} else if err := token.validate(); err != nil {
		if err := r.deleteToken(conn, tokenType, token); err != nil {
			logging.SystemErrorf("revoke expired %s [%s] failed: %s", tokenType.name(), tokenType.get(token), err)
		}

		return nil, errors.Wrap(err, "validate token")
	} else {
		return &middleware.Authority{
			UserID:  token.UserID,
			IsAdmin: false,
		}, nil
	}
}

func (r *Redis) FindAnyUserID(ctx context.Context) (string, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return "", err
	}

	defer closeQuietly(conn)

	if userIDs, err := redis.StringMap(conn.Do("HGETALL", usersIndexKey)); errors.Is(err, redis.ErrNil) {
		return "", ErrUserNotFound
	} else if err != nil {
		return "", errors.Wrap(err, "find users")
	} else {
		for _, userID := range userIDs {
			return userID, nil
		}

		return "", ErrUserNotFound
	}
}

func (r *Redis) HasUsers(ctx context.Context) (bool, error) {
	if _, err := r.FindAnyUserID(ctx); errors.Is(err, ErrUserNotFound) {
		return false, nil
	} else if err != nil {
		return false, errors.Wrap(err, "find any user id")
	} else {
		return true, nil
	}
}

func (r *Redis) RefreshToken(ctx context.Context, token string) (*openapi.TokensResponse, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return nil, err
	}

	defer closeQuietly(conn)

	tokenType := refreshTokenType
	if token, err := r.getToken(conn, tokenType, token); err != nil {
		return nil, errors.Wrap(err, "find token")
	} else if err := token.validate(); err != nil {
		if errors.Is(err, errExpiredToken) {
			if err := r.revokeToken(conn, token); err != nil {
				logging.SystemErrorf("revoke expired %s [%s] failed: %s", tokenType.name(), token, err)
			}
		}

		return nil, errors.Wrap(err, "validate token")
	} else if err := r.revokeToken(conn, token); err != nil {
		return nil, errors.Wrap(err, "revoke token")
	} else if tokenPair, err := r.generateTokenPair(conn, token.UserID); err != nil {
		return nil, errors.Wrap(err, "generate token pair")
	} else {
		return tokenPair, nil
	}
}

func (r *Redis) SignOut(ctx context.Context, token string) error {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return err
	}

	defer closeQuietly(conn)

	if token, err := r.getToken(conn, accessTokenType, token); errors.Is(err, errUnknownToken) {
		return nil
	} else if err != nil {
		return errors.Wrap(err, "get token")
	} else if err := r.revokeToken(conn, token); err != nil {
		return errors.Wrap(err, "revoke token")
	} else {
		return nil
	}
}

func (r *Redis) AutoSignUp(ctx context.Context, email, callback string) (string, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return "", err
	}

	defer closeQuietly(conn)

	if userID, err := r.createUser(conn, email, uuid.NewV4().String(), true); errors.Is(err, ErrUserExists) {
		return userID, nil
	} else if err != nil {
		return "", errors.Wrap(err, "create user")
	} else if err := r.sendResetPasswordLink(conn, userID, email, callback); err != nil {
		return userID, errors.Wrap(err, "send reset password link")
	} else {
		return userID, nil
	}
}

func (r *Redis) SignIn(ctx context.Context, email, password string) (*openapi.TokensResponse, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return nil, err
	}

	defer closeQuietly(conn)

	if userID, err := r.getUserIDByEmail(conn, email); err != nil {
		return nil, errors.Wrap(err, "find user id by email")
	} else if hashedPassword, err := redis.String(
		conn.Do("HGET", userKey(userID), userHashedPasswordField),
	); errors.Is(err, redis.ErrNil) {
		logging.SystemErrorf("User [%s] exists in [%s], but not under [%s]", userID, usersIndexKey, userKey(userID))
		return nil, ErrUserNotFound
	} else if err != nil {
		return nil, errors.Wrap(err, "get user by id")
	} else if err := r.passwordEncoder.Compare(hashedPassword, password); err != nil {
		return nil, errors.Wrap(err, "check password")
	} else if tokenPair, err := r.generateTokenPair(conn, userID); err != nil {
		return nil, errors.Wrap(err, "generate token pair")
	} else {
		return tokenPair, nil
	}
}

func (r *Redis) SignUp(ctx context.Context, email, password string) (*openapi.TokensResponse, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return nil, err
	}

	defer closeQuietly(conn)

	if userID, err := r.createUser(conn, email, password, false); err != nil {
		return nil, errors.Wrap(err, "sign up")
	} else if tokenPair, err := r.generateTokenPair(conn, userID); err != nil {
		return nil, errors.Wrap(err, "generate token pair")
	} else {
		return tokenPair, nil
	}
}

func (r *Redis) SendResetPasswordLink(ctx context.Context, email, callback string) error {
	if !r.mailSender.IsConfigured() {
		return errMailServiceNotConfigured
	}

	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return err
	}

	defer closeQuietly(conn)

	if userID, err := r.getUserIDByEmail(conn, email); err != nil {
		return errors.Wrap(err, "get user id by email")
	} else if err := r.sendResetPasswordLink(conn, userID, email, callback); err != nil {
		return errors.Wrap(err, "send reset password link")
	} else {
		return nil
	}
}

func (r *Redis) ResetPassword(ctx context.Context, resetID, newPassword string) (*openapi.TokensResponse, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return nil, err
	}

	defer closeQuietly(conn)

	resetKey := resetKey(resetID)
	if userID, err := redis.String(conn.Do("GET", resetKey)); errors.Is(err, redis.ErrNil) {
		return nil, errors.New("unknown reset id")
	} else if err != nil {
		return nil, errors.Wrap(err, "get user id by reset id")
	} else if tokenPair, err := r.changePassword(conn, userID, newPassword); err != nil {
		return nil, errors.Wrap(err, "change password")
	} else if _, err := conn.Do("DEL", resetKey); err != nil {
		return nil, errors.Wrap(err, "delete reset id")
	} else {
		return tokenPair, nil
	}
}

func (r *Redis) ChangePassword(ctx context.Context, userID, newPassword string) (*openapi.TokensResponse, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return nil, err
	}

	defer closeQuietly(conn)

	if tokenPair, err := r.changePassword(conn, userID, newPassword); err != nil {
		return nil, errors.Wrap(err, "change password")
	} else {
		return tokenPair, nil
	}
}

func (r *Redis) ChangeEmail(ctx context.Context, oldEmail, newEmail string) (string, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return "", err
	}

	defer closeQuietly(conn)

	userID, err := r.getUserIDByEmail(conn, oldEmail)
	if err != nil {
		return "", errors.Wrap(err, "get user by old email")
	}

	if _, err := r.getUserIDByEmail(conn, newEmail); errors.Is(err, ErrUserNotFound) {
		// is ok
	} else if err != nil {
		return "", errors.Wrapf(err, "verify new email not used")
	} else {
		return "", ErrUserExists
	}

	userKey := userKey(userID)
	if _, err := conn.Do("HSET", userKey,
		userEmailField, newEmail,
	); err != nil {
		return "", errors.Wrapf(err, "update %s", userEmailField)
	} else if _, err := conn.Do("HSET", usersIndexKey, newEmail, userID); err != nil {
		return "", errors.Wrapf(err, "update %s", usersIndexKey)
	} else if _, err := conn.Do("HDEL", usersIndexKey, oldEmail); err != nil {
		return "", errors.Wrapf(err, "remove previous email association from %s", usersIndexKey)
	} else {
		return userID, nil
	}
}

func (r *Redis) ListUsers(ctx context.Context) ([]openapi.UserBasicInfo, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return nil, err
	}

	defer closeQuietly(conn)

	if values, err := redis.StringMap(conn.Do("HGETALL", usersIndexKey)); err != nil {
		return nil, errors.Wrapf(err, "get all users")
	} else {
		result := make([]openapi.UserBasicInfo, 0, len(values))
		for email, userID := range values {
			result = append(result, openapi.UserBasicInfo{
				Id:    userID,
				Email: email,
			})
		}

		return result, nil
	}
}

func (r *Redis) CreateUser(ctx context.Context, email string) (*handlers.CreatedUser, error) {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return nil, err
	}

	defer closeQuietly(conn)

	if userID, err := r.createUser(conn, email, uuid.NewV4().String(), false); err != nil {
		return nil, errors.Wrapf(err, "create user")
	} else if resetID, err := r.generateResetID(conn, userID); err != nil {
		return nil, errors.Wrapf(err, "generate reset password id")
	} else {
		return &handlers.CreatedUser{
			ID:      userID,
			ResetID: resetID,
		}, nil
	}
}

func (r *Redis) DeleteUser(ctx context.Context, userID string) error {
	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return err
	}

	defer closeQuietly(conn)

	userKey := userKey(userID)
	if email, err := redis.String(conn.Do("HGET", userKey, userEmailField)); errors.Is(err, redis.ErrNil) {
		return ErrUserNotFound
	} else if err := r.revokeTokens(conn, userID); err != nil {
		return errors.Wrap(err, "revoke tokens")
	} else if err != nil {
		return errors.Wrap(err, "get user email by id")
	} else if _, err := conn.Do("DEL", userKey); err != nil {
		return errors.Wrap(err, "remove user data")
	} else if _, err := conn.Do("HDEL", usersIndexKey, email); err != nil {
		return errors.Wrapf(err, "remove %s from %s", email, usersIndexKey)
	} else {
		return nil
	}
}

func (r *Redis) UpdateUser(ctx context.Context, userID string, newPassword *string, forcePasswordChange bool) error {
	if newPassword == nil && !forcePasswordChange {
		return nil
	}

	conn, err := r.redisPool.GetContext(ctx)
	if err != nil {
		return err
	}

	defer closeQuietly(conn)

	userKey := userKey(userID)
	if _, err := conn.Do("HGET", userKey, userHashedPasswordField); errors.Is(err, redis.ErrNil) {
		return ErrUserNotFound
	}

	args := make([]interface{}, 1, 3)
	args[0] = userKey
	if forcePasswordChange {
		args = append(args, userForceChangePasswordField, true)
	}

	if newPassword != nil {
		if hashedPassword, err := r.passwordEncoder.Encode(*newPassword); err != nil {
			return errors.Wrap(err, "encode new password")
		} else {
			args = append(args, userHashedPasswordField, hashedPassword)
		}

		if err := r.revokeTokens(conn, userID); err != nil {
			return errors.Wrap(err, "revoke tokens")
		}
	}

	if _, err := conn.Do("HSET", args...); err != nil {
		return errors.Wrap(err, "update user data")
	} else {
		return nil
	}
}

func (r *Redis) sendResetPasswordLink(conn redis.Conn, userID, email, callback string) error {
	if resetID, err := r.generateResetID(conn, userID); err != nil {
		return errors.Wrap(err, "generate reset id")
	} else if err := r.mailSender.SendResetPassword(email, strings.ReplaceAll(callback, "{{token}}", resetID)); err != nil {
		return errors.Wrap(err, "send reset password")
	} else {
		return nil
	}
}

func (r *Redis) generateResetID(conn redis.Conn, userID string) (string, error) {
	resetID := "reset-" + uuid.NewV4().String()
	if _, err := conn.Do("SET", resetKey(resetID), userID, "EX", resetIDTTL); err != nil {
		return "", errors.Wrap(err, "persist reset id")
	} else {
		return resetID, nil
	}
}

func (r *Redis) createUser(conn redis.Conn, email, password string, requireMailService bool) (string, error) {
	if userID, err := r.getUserIDByEmail(conn, email); err == nil {
		return userID, ErrUserExists
	} else if !errors.Is(err, ErrUserNotFound) {
		return "", errors.Wrap(err, "get user by email")
	} else if requireMailService && !r.mailSender.IsConfigured() {
		return "", errMailServiceNotConfigured
	} else if hashedPassword, err := r.passwordEncoder.Encode(password); err != nil {
		return "", errors.Wrap(err, "encode password")
	} else {
		id := "user-" + uuid.NewV4().String()
		if _, err := conn.Do("HSET", userKey(id),
			userIDField, id,
			userEmailField, email,
			userHashedPasswordField, hashedPassword,
		); err != nil {
			return "", errors.Wrap(err, "create user")
		} else if _, err := conn.Do("HSET", usersIndexKey, email, id); err != nil {
			return "", errors.Wrapf(err, "update %s", usersIndexKey)
		} else {
			return id, nil
		}
	}
}

func (r *Redis) changePassword(conn redis.Conn, userID, newPassword string) (*openapi.TokensResponse, error) {
	if hashedPassword, err := r.passwordEncoder.Encode(newPassword); err != nil {
		return nil, errors.Wrap(err, "encode password")
	} else if err := r.revokeTokens(conn, userID); err != nil {
		return nil, errors.Wrap(err, "revoke user tokens")
	} else if _, err := conn.Do("HSET", userKey(userID),
		userHashedPasswordField, hashedPassword,
		userForceChangePasswordField, false,
	); err != nil {
		return nil, errors.Wrap(err, "update password")
	} else if tokenPair, err := r.generateTokenPair(conn, userID); err != nil {
		return nil, errors.Wrap(err, "generate new token pair")
	} else {
		return tokenPair, nil
	}
}

func (r *Redis) generateTokenPair(conn redis.Conn, userID string) (*openapi.TokensResponse, error) {
	now := timestamp.Now()
	access := newRedisToken(now, userID, accessTokenType)
	refresh := newRedisToken(now, userID, refreshTokenType)

	// link tokens
	access.RefreshToken, refresh.AccessToken = refresh.RefreshToken, access.AccessToken

	if err := r.saveToken(conn, accessTokenType, access); err != nil {
		return nil, errors.Wrap(err, "save access token")
	} else if err := r.saveToken(conn, refreshTokenType, refresh); err != nil {
		return nil, errors.Wrap(err, "save refresh token")
	} else {
		return &openapi.TokensResponse{
			UserId:       userID,
			AccessToken:  access.AccessToken,
			RefreshToken: refresh.RefreshToken,
		}, nil
	}
}

func (r *Redis) getUserIDByEmail(conn redis.Conn, email string) (string, error) {
	if userID, err := redis.String(conn.Do("HGET", usersIndexKey, email)); errors.Is(err, redis.ErrNil) {
		return "", ErrUserNotFound
	} else if err != nil {
		return "", errors.Wrap(err, "find user")
	} else {
		return userID, nil
	}
}

func (r *Redis) saveToken(conn redis.Conn, tokenType redisTokenType, token *redisToken) error {
	if data, err := json.Marshal(token); err != nil {
		return errors.Wrap(err, "marshal token")
	} else if _, err := conn.Do("HSET", tokenType.key(), tokenType.get(token), data); err != nil {
		return errors.Wrap(err, "persist token")
	} else {
		return nil
	}
}

func (r *Redis) revokeTokens(conn redis.Conn, userID string) error {
	if err := r.revokeTokenType(conn, userID, accessTokenType); err != nil {
		return errors.Wrap(err, "revoke access tokens")
	} else if err := r.revokeTokenType(conn, userID, refreshTokenType); err != nil {
		return errors.Wrap(err, "revoke refresh tokens")
	} else {
		return nil
	}
}

func (r *Redis) revokeTokenType(conn redis.Conn, userID string, tokenType redisTokenType) error {
	if data, err := redis.StringMap(conn.Do("HGETALL", tokenType.key())); errors.Is(err, redis.ErrNil) {
		return nil
	} else if err != nil {
		return errors.Wrap(err, "get tokens")
	} else {
		for _, data := range data {
			var token redisToken
			if err := json.Unmarshal([]byte(data), &token); err != nil {
				err = errors.Wrapf(err, "malformed token data [%s] for user [%s]", data, userID)
				logging.Info(err)
				return err
			} else if token.UserID != userID {
				continue
			} else if err := r.revokeToken(conn, &token); err != nil {
				err = errors.Wrapf(err, "revoke token [%v]", token)
				logging.Info(err)
				return err
			}
		}

		return nil
	}
}

func (r *Redis) revokeToken(conn redis.Conn, token *redisToken) error {
	if err := r.deleteToken(conn, accessTokenType, token); err != nil {
		return err
	} else if err := r.deleteToken(conn, refreshTokenType, token); err != nil {
		return err
	} else {
		return nil
	}
}

func (r *Redis) deleteToken(conn redis.Conn, tokenType redisTokenType, token *redisToken) error {
	if _, err := conn.Do("HDEL", tokenType.key(), tokenType.get(token)); err != nil {
		return errors.Wrapf(err, "delete %s", tokenType.name())
	} else {
		return nil
	}
}

func (r *Redis) getToken(conn redis.Conn, tokenType redisTokenType, token string) (*redisToken, error) {
	var result redisToken
	if data, err := redis.Bytes(conn.Do("HGET", tokenType.key(), token)); err == redis.ErrNil {
		return nil, errUnknownToken
	} else if err != nil {
		return nil, errors.Wrap(err, "get token")
	} else if len(data) == 0 {
		return nil, errUnknownToken
	} else if err := json.Unmarshal(data, &result); err != nil {
		err = errors.Wrapf(err, "malformed token [%s] data [%s]", token, string(data))
		logging.SystemError(err)
		return nil, err
	} else {
		return &result, nil
	}
}

func userKey(userID string) string {
	return "user#" + userID
}

func resetKey(resetID string) string {
	return "password_reset#" + resetID
}

func closeQuietly(conn redis.Conn) {
	_ = conn.Close()
}
