package httputil

import (
	"errors"
	"net/http"
)

type AppError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Message
}

func (e *AppError) Unwrap() error {
	return e.Err
}

func NewBadRequest(message string) *AppError {
	return &AppError{Status: http.StatusBadRequest, Code: "BAD_REQUEST", Message: message}
}

func NewUnauthorized(message string) *AppError {
	return &AppError{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: message}
}

func NewForbidden(message string) *AppError {
	return &AppError{Status: http.StatusForbidden, Code: "FORBIDDEN", Message: message}
}

func NewNotFound(message string) *AppError {
	return &AppError{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: message}
}

func NewConflict(message string) *AppError {
	return &AppError{Status: http.StatusConflict, Code: "CONFLICT", Message: message}
}

func NewInternal(err error) *AppError {
	return &AppError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "internal server error", Err: err}
}

func NewRateLimited() *AppError {
	return &AppError{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED", Message: "too many requests"}
}

func HandleError(w http.ResponseWriter, err error) {
	var appErr *AppError
	if errors.As(err, &appErr) {
		JSONError(w, appErr.Status, appErr.Code, appErr.Message)
		return
	}
	JSONError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
}
