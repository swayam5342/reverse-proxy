package config

import (
	"strings"
	"testing"
)

func TestValidate_JWTCookie(t *testing.T) {
	t.Setenv("JWT_SECRET", "s")
	jwtAuth := AuthConfig{Enabled: true, Mode: "jwt", JWTSecretEnv: "JWT_SECRET", JWTCookie: "session"}

	tests := []struct {
		name    string
		auth    AuthConfig
		cors    CORSConfig
		wantErr string
	}{
		{"same-origin frontend, no cors", jwtAuth, CORSConfig{}, ""},
		{"cross-origin with credentials", jwtAuth, CORSConfig{Enabled: true, AllowedOrigins: []string{"https://app.example.com"}, AllowCredentials: true}, ""},
		{"cors without credentials", jwtAuth, CORSConfig{Enabled: true, AllowedOrigins: []string{"https://app.example.com"}}, "allow_credentials"},
		{"cors wildcard", jwtAuth, CORSConfig{Enabled: true, AllowedOrigins: []string{"*"}}, `"*"`},
		{"cookie on api_key route", AuthConfig{Enabled: true, Mode: "api_key", APIKeysEnv: "JWT_SECRET", JWTCookie: "session"}, CORSConfig{}, "jwt_cookie"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Routes[0].Auth = tt.auth
			cfg.Routes[0].CORS = tt.cors
			err := cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
