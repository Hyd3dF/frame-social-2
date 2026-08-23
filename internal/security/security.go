package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Tokens struct {
	AccessToken           string `json:"accessToken"`
	RefreshToken          string `json:"refreshToken"`
	RefreshTokenExpiresAt string `json:"refreshTokenExpiresAt"`
}

func OTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func OTPHash(pepper, phone, code string) string {
	mac := hmac.New(sha256.New, []byte(pepper))
	_, _ = mac.Write([]byte(phone + ":" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

func VerifyOTP(pepper, phone, code, expected string) bool {
	actual, errA := hex.DecodeString(OTPHash(pepper, phone, code))
	wanted, errB := hex.DecodeString(expected)
	return errA == nil && errB == nil && hmac.Equal(actual, wanted)
}

func RefreshToken() (string, error) {
	value := make([]byte, 48)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func TokenHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func AccessToken(secret, subject string, minutes int) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   subject,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(minutes) * time.Minute)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func ParseAccessToken(secret, token string) (string, error) {
	parsed, err := jwt.ParseWithClaims(token, &jwt.RegisteredClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil || !parsed.Valid {
		return "", fmt.Errorf("invalid access token")
	}
	claims, ok := parsed.Claims.(*jwt.RegisteredClaims)
	if !ok || claims.Subject == "" {
		return "", fmt.Errorf("missing token subject")
	}
	return claims.Subject, nil
}

func PairKey(left, right string) string {
	values := []string{left, right}
	sort.Strings(values)
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])
}
