// Package tlsx builds the gateway's TLS configuration: automatic Let's
// Encrypt certificates (ACME) or a certificate/key pair from disk that is
// reloaded when renewed, plus the plaintext handler that redirects to
// HTTPS and answers ACME HTTP-01 challenges.
package tlsx

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// FileReloader serves a certificate/key pair from disk and picks up
// renewals without a restart. At most once per checkEvery, during a
// handshake, it compares the files' modification times and reloads if
// either changed; no background goroutine is involved. A failed reload
// keeps serving the last good certificate and reports the error.
type FileReloader struct {
	certFile, keyFile string
	checkEvery        time.Duration
	onErr             func(error)

	mu              sync.Mutex
	cert            *tls.Certificate
	certMod, keyMod time.Time
	lastCheck       time.Time
}

// NewFileReloader loads the pair once, returning an error if that fails.
// onErr (optional) is called for every failed reload.
func NewFileReloader(certFile, keyFile string, checkEvery time.Duration, onErr func(error)) (*FileReloader, error) {
	r := &FileReloader{certFile: certFile, keyFile: keyFile, checkEvery: checkEvery, onErr: onErr}
	certMod, keyMod, err := r.mtimes()
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsx: loading %s / %s: %w", certFile, keyFile, err)
	}
	r.cert, r.certMod, r.keyMod, r.lastCheck = &cert, certMod, keyMod, time.Now()
	return r, nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *FileReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastCheck) >= r.checkEvery {
		r.lastCheck = time.Now()
		r.reloadIfChangedLocked()
	}
	return r.cert, nil
}

func (r *FileReloader) reloadIfChangedLocked() {
	certMod, keyMod, err := r.mtimes()
	if err != nil {
		r.report(err)
		return
	}
	if certMod.Equal(r.certMod) && keyMod.Equal(r.keyMod) {
		return
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		// Leave the recorded mtimes alone so the next check retries (the
		// renewal may have been caught half-written).
		r.report(fmt.Errorf("tlsx: reloading %s / %s: %w", r.certFile, r.keyFile, err))
		return
	}
	r.cert, r.certMod, r.keyMod = &cert, certMod, keyMod
}

func (r *FileReloader) mtimes() (certMod, keyMod time.Time, err error) {
	ci, err := os.Stat(r.certFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("tlsx: %w", err)
	}
	ki, err := os.Stat(r.keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("tlsx: %w", err)
	}
	return ci.ModTime(), ki.ModTime(), nil
}

func (r *FileReloader) report(err error) {
	if r.onErr != nil {
		r.onErr(err)
	}
}
