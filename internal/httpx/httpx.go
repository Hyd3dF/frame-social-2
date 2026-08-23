package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const maxBodyBytes = 1 << 20

type ErrorResponse struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func Decode(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		Error(w, http.StatusBadRequest, "invalid_request", "Gönderilen bilgiler geçersiz.")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		Error(w, http.StatusBadRequest, "invalid_request", "Yalnızca bir JSON nesnesi gönderilebilir.")
		return false
	}
	return true
}

func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, ErrorResponse{Error: APIError{Code: code, Message: message}})
}
