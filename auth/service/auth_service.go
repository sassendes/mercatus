package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"mercatus/auth/model"
	"mercatus/auth/repository"
)

var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidCode        = errors.New("invalid or expired code")
	ErrInvalidInput       = errors.New("invalid input")
	ErrUserNotFound       = errors.New("user not found")
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }
func (e *ValidationError) Unwrap() error { return ErrInvalidInput }

const refreshTTL = 30 * 24 * time.Hour

type CodeDelivery func(ctx context.Context, userID int64, email, code string)

type AuthService struct {
	users       *repository.UserRepository
	tokens      *repository.TokenRepository
	dummyHash   string
	deliverCode CodeDelivery
}

func (s *AuthService) SetCodeDelivery(fn CodeDelivery) { s.deliverCode = fn }

func NewAuthService(users *repository.UserRepository, tokens *repository.TokenRepository) *AuthService {
	dummy, _ := HashPassword("dummy-password-for-timing")
	return &AuthService{users: users, tokens: tokens, dummyHash: dummy}
}

func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validateRegistration(req *model.RegisterRequest) error {
	req.Email = NormalizeEmail(req.Email)
	req.Username = strings.TrimSpace(req.Username)
	addr, err := mail.ParseAddress(req.Email)
	if err != nil || addr.Address != req.Email || len(req.Email) > 254 {
		return &ValidationError{"a valid email address is required"}
	}
	if n := utf8.RuneCountInString(req.Username); n < 2 || n > 50 {
		return &ValidationError{"name must be between 2 and 50 characters"}
	}
	if n := utf8.RuneCountInString(req.Password); n < 8 || n > 128 {
		return &ValidationError{"password must be between 8 and 128 characters"}
	}
	switch req.Role {
	case "":
		req.Role = model.RoleBuyer
	case model.RoleBuyer, model.RoleSeller:
	default:
		return &ValidationError{"role must be buyer or seller"}
	}
	return nil
}

func newOpaqueToken() (plain, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	plain = base64.RawURLEncoding.EncodeToString(b)
	return plain, HashToken(plain), nil
}

func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (s *AuthService) Register(ctx context.Context, req *model.RegisterRequest) (*model.User, error) {
	if err := validateRegistration(req); err != nil {
		return nil, err
	}
	release, err := acquire(ctx)
	if err != nil {
		return nil, err
	}
	hashed, err := HashPassword(req.Password)
	release()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	user := &model.User{
		Username: req.Username, Email: req.Email, PasswordHash: hashed,
		Role: req.Role, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.users.CreateUser(ctx, user); err != nil {
		if errors.Is(err, repository.ErrDuplicateEmail) {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	code, err := generateCode()
	if err != nil {
		return nil, err
	}
	ev := &model.EmailVerification{UserID: user.ID, Code: code, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now}
	if err := s.tokens.CreateEmailVerification(ctx, ev); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "email verification code issued", "user_id", user.ID)
	if s.deliverCode != nil {
		s.deliverCode(ctx, user.ID, user.Email, code)
	}
	return user, nil
}

func (s *AuthService) issue(ctx context.Context, user *model.User, jwtMgr *JWTManager, rotateFrom string) (*model.AuthResponse, error) {
	access, err := jwtMgr.Generate(user)
	if err != nil {
		return nil, err
	}
	plain, hash, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rt := &model.RefreshToken{UserID: user.ID, TokenHash: hash, ExpiresAt: now.Add(refreshTTL), CreatedAt: now}
	if rotateFrom != "" {
		err = s.tokens.Rotate(ctx, rotateFrom, rt)
	} else {
		err = s.tokens.CreateRefreshToken(ctx, rt)
	}
	if err != nil {
		if errors.Is(err, repository.ErrTokenNotActive) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	return &model.AuthResponse{
		AccessToken:  access,
		RefreshToken: plain,
		ExpiresIn:    int64(jwtMgr.AccessDuration().Seconds()),
		User:         user.ToPublic(),
	}, nil
}

func (s *AuthService) Login(ctx context.Context, req *model.LoginRequest, jwtMgr *JWTManager) (*model.AuthResponse, error) {
	email := NormalizeEmail(req.Email)
	if email == "" || req.Password == "" || len(req.Password) > 128 {
		return nil, ErrInvalidCredentials
	}
	user, err := s.users.FindByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	release, err := acquire(ctx)
	if err != nil {
		return nil, err
	}
	encoded := s.dummyHash
	if user != nil {
		encoded = user.PasswordHash
	}
	ok := VerifyPassword(req.Password, encoded)
	release()
	if user == nil || !ok {
		return nil, ErrInvalidCredentials
	}
	return s.issue(ctx, user, jwtMgr, "")
}

func (s *AuthService) Refresh(ctx context.Context, refreshToken string, jwtMgr *JWTManager) (*model.AuthResponse, error) {
	if refreshToken == "" {
		return nil, ErrInvalidCredentials
	}
	hash := HashToken(refreshToken)
	stored, err := s.tokens.FindRefreshToken(ctx, hash)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, ErrInvalidCredentials
	}
	if stored.Revoked {
		slog.WarnContext(ctx, "revoked refresh token reused, revoking all sessions", "user_id", stored.UserID)
		_ = s.tokens.RevokeAllForUser(ctx, stored.UserID)
		return nil, ErrInvalidCredentials
	}
	if time.Now().After(stored.ExpiresAt) {
		return nil, ErrInvalidCredentials
	}
	user, err := s.users.FindByID(ctx, stored.UserID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, ErrInvalidCredentials
	}
	return s.issue(ctx, user, jwtMgr, hash)
}

func (s *AuthService) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	return s.tokens.RevokeRefreshToken(ctx, HashToken(refreshToken))
}

func (s *AuthService) Me(ctx context.Context, userID int64) (*model.User, error) {
	u, err := s.users.FindByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	return u, nil
}

func (s *AuthService) VerifyEmail(ctx context.Context, userID int64, code string) error {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return ErrInvalidCode
	}
	ok, err := s.tokens.ConsumeEmailVerification(ctx, userID, code)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCode
	}
	return nil
}

func (s *AuthService) PruneTokens(ctx context.Context) (int64, error) {
	return s.tokens.DeleteExpired(ctx)
}
