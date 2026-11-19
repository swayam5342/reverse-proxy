package config

import (
	"strings"
	"testing"
)

func TestValidate_CORS(t *testing.T) {
	tests := []struct {
		name    string
		cors    CORSConfig
		wantErr string
	}{
		{"valid", CORSConfig{Enabled: true, AllowedOrigins: []string{"https://app.example.com", "http://localhost:5173"}}, ""},
		{"wildcard without credentials", CORSConfig{Enabled: true, AllowedOrigins: []string{"*"}}, ""},
		{"disabled ignores fields", CORSConfig{AllowedOrigins: []string{"garbage"}}, ""},
		{"no origins", CORSConfig{Enabled: true}, "cors.allowed_origins"},
		{"wildcard with credentials", CORSConfig{Enabled: true, AllowedOrigins: []string{"*"}, AllowCredentials: true}, "allow_credentials"},
		{"origin without scheme", CORSConfig{Enabled: true, AllowedOrigins: []string{"app.example.com"}}, "scheme://host"},
		{"origin with path", CORSConfig{Enabled: true, AllowedOrigins: []string{"https://app.example.com/login"}}, "scheme://host"},
		{"negative max_age", CORSConfig{Enabled: true, AllowedOrigins: []string{"https://a.example"}, MaxAge: -1}, "cors.max_age"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
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
