package domain

import "errors"

// Доменные (sentinel) ошибки. Транспортный слой сопоставляет их с HTTP-кодами
// через errors.Is — так HTTP-статусы не «протекают» в бизнес-логику.
var (
	ErrNotFound     = errors.New("link not found")
	ErrExpired      = errors.New("link expired")
	ErrCodeTaken    = errors.New("code already taken")
	ErrInvalidURL   = errors.New("invalid url")
	ErrInvalidAlias = errors.New("invalid alias")
	ErrCacheMiss    = errors.New("cache miss")
	ErrRateLimited  = errors.New("rate limit exceeded")
)
