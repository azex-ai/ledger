// Package anchordev is a local-filesystem implementation of core.Anchor
// (design doc §8.3, P6 of the integrity-hardening wave). DEV / TEST ONLY --
// unlike authdev's ed25519 Attestor (which Team Lead deliberately promoted
// to a production-ready default for a monolith deployment), a file on the
// same host as the ledger's own database is not an equivalent
// simplification here: the whole point of an external anchor is living
// somewhere the ledger's DB credentials cannot reach, so a local file
// defeats the purpose it exists to serve. Production requires a real
// carrier (an object-lock bucket in a separate cloud account, at minimum)
// that this library deliberately does not ship (integrity contracts §7:
// the carrier is a genuinely unresolved deployment choice, not deferred
// out of laziness).
//
// Must be constructed explicitly; nothing in this package or core wires it
// in automatically.
package anchordev

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/azex-ai/ledger/core"
)

// LocalFileAnchor is a core.Anchor that remembers only the latest
// (seq, head) pair, in a single small file. It does not keep a history --
// Publish accepts only the next sequential seq (Head().seq + 1) as a new
// entry, or an exact idempotent replay of the current one; anything else
// is refused rather than silently accepted (a dev tool that lied about
// ordering would be worse than one that refused).
type LocalFileAnchor struct {
	mu   sync.Mutex
	path string
}

var _ core.Anchor = (*LocalFileAnchor)(nil)

// NewLocalFileAnchorForDevelopment returns a LocalFileAnchor persisting to
// path. The parent directory must already exist; the file itself is created
// on the first Publish call.
//
// The name is the gate (2026-09-02 audit, tamper-evident.md m-1 / C-m1).
// This package's prose has always said DEV/TEST ONLY, and prose is not a
// gate: a composition root wiring `anchordev.NewLocalFileAnchor(path)` into
// production read like any other adapter, and the worker's startup log
// reported `attestation_anchor: true` for it exactly as it did for a real
// external carrier. Every other dev-only feature in this library requires
// the deployer to say so out loud (dev_credit needs ENV=dev plus
// DEV_CREDIT_ENABLED plus an explicit preset install), so this one does too
// -- here, by making the words "ForDevelopment" appear at the call site,
// which cannot be forgotten silently the way a comment can. The runtime half
// is service.StartupReport.AttestationAnchorType, which now names the
// anchor's type and warns when it is this one.
func NewLocalFileAnchorForDevelopment(path string) *LocalFileAnchor {
	return &LocalFileAnchor{path: path}
}

// Publish implements core.Anchor.
func (a *LocalFileAnchor) Publish(ctx context.Context, seq int64, head []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	curSeq, curHead, err := a.readLocked()
	if err != nil {
		return err
	}

	switch {
	case seq == curSeq && curSeq != 0:
		if !bytes.Equal(curHead, head) {
			return fmt.Errorf("anchordev: seq %d already published with a different head", seq)
		}
		return nil // idempotent replay
	case seq == curSeq+1:
		return a.writeLocked(seq, head)
	default:
		return fmt.Errorf("anchordev: seq %d is not the next sequential seq (current head is seq %d): %w", seq, curSeq, core.ErrInvalidInput)
	}
}

// Head implements core.Anchor. Returns (0, nil, nil) if nothing has been
// published yet.
func (a *LocalFileAnchor) Head(ctx context.Context) (int64, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readLocked()
}

func (a *LocalFileAnchor) readLocked() (int64, []byte, error) {
	data, err := os.ReadFile(a.path)
	if os.IsNotExist(err) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("anchordev: read %s: %w", a.path, err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) != 2 {
		return 0, nil, fmt.Errorf("anchordev: %s is malformed (expected 2 lines, got %d)", a.path, len(lines))
	}
	seq, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("anchordev: %s: parse seq: %w", a.path, err)
	}
	head, err := hex.DecodeString(lines[1])
	if err != nil {
		return 0, nil, fmt.Errorf("anchordev: %s: decode head: %w", a.path, err)
	}
	return seq, head, nil
}

// writeLocked persists (seq, head) atomically: write to a temp file in the
// same directory, then rename over the target -- a crash mid-write leaves
// the previous, still-valid file in place rather than a corrupt partial
// one.
func (a *LocalFileAnchor) writeLocked(seq int64, head []byte) error {
	content := fmt.Sprintf("%d\n%s\n", seq, hex.EncodeToString(head))
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("anchordev: write temp file: %w", err)
	}
	if err := os.Rename(tmp, a.path); err != nil {
		return fmt.Errorf("anchordev: rename temp file: %w", err)
	}
	return nil
}

// EnsureDir is a small test/dev convenience: creates path's parent
// directory (0o700) if it does not already exist.
func EnsureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}
