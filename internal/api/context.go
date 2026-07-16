package api

import (
	"context"

	"github.com/zawnk/later/internal/config"
)

type contextKey string

const tokenKey contextKey = "token"

func contextWithToken(ctx context.Context, t config.Token) context.Context {
	return context.WithValue(ctx, tokenKey, t)
}

func tokenFromContext(ctx context.Context) config.Token {
	t, _ := ctx.Value(tokenKey).(config.Token)
	return t
}
