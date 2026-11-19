package main

import (
	"net/http"
	"time"
)

// defaultHealthcheckURL and defaultHealthcheckTimeout are used by the
// -healthcheck flag (container healthchecks; the image has no shell/curl).
const (
	defaultHealthcheckURL     = "http://127.0.0.1:9090/healthz"
	defaultHealthcheckTimeout = 2 * time.Second
)

// healthcheck GETs url and returns the process exit code: 0 for a 200
// response, 1 for anything else (error, timeout, other status).
func healthcheck(url string, timeout time.Duration) int {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
