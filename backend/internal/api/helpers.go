package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/nyaruka/phonenumbers"
)

const maxBodyBytes = 1 << 20

var usernamePattern = regexp.MustCompile(`^[a-z0-9_]{3,30}$`)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error apiError `json:"error"`
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_request", "Gönderilen bilgiler geçersiz.")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		respondError(w, http.StatusBadRequest, "invalid_request", "Yalnızca bir JSON nesnesi gönderilebilir.")
		return false
	}
	return true
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func respondError(w http.ResponseWriter, status int, code, message string) {
	respondJSON(w, status, errorResponse{Error: apiError{Code: code, Message: message}})
}

func normalizePhone(value, countryCode string) (string, error) {
	region := strings.ToUpper(strings.TrimSpace(countryCode))
	parsed, err := phonenumbers.Parse(strings.TrimSpace(value), region)
	if err != nil || !phonenumbers.IsValidNumber(parsed) {
		return "", fmt.Errorf("invalid phone number")
	}
	return phonenumbers.Format(parsed, phonenumbers.E164), nil
}

func validRecord(value, table string) bool {
	return strings.HasPrefix(value, table+":") && len(value) > len(table)+1 && !strings.ContainsAny(value, " ;'\"")
}

func validVerifyRequest(input verifyRequest) bool {
	return len(input.Code) == 6 && len(strings.TrimSpace(input.DeviceID)) >= 8 && len(input.DeviceID) <= 200 && (input.Platform == "ios" || input.Platform == "android")
}

func deviceName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Unknown device"
	}
	if len([]rune(value)) > 100 {
		value = string([]rune(value)[:100])
	}
	return value
}

func stringValue(values map[string]any, key string) string {
	v, _ := values[key].(string)
	return v
}

func pointerString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func boolValue(v *bool) bool {
	return v != nil && *v
}
