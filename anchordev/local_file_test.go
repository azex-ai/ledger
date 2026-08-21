package anchordev

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLocalFileAnchor_HeadOnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	a := NewLocalFileAnchor(filepath.Join(dir, "anchor.txt"))

	seq, head, err := a.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: unexpected error: %v", err)
	}
	if seq != 0 || head != nil {
		t.Errorf("Head on empty anchor = (%d, %x), want (0, nil)", seq, head)
	}
}

func TestLocalFileAnchor_PublishAndHead(t *testing.T) {
	dir := t.TempDir()
	a := NewLocalFileAnchor(filepath.Join(dir, "anchor.txt"))
	ctx := context.Background()

	head1 := make([]byte, 32)
	head1[0] = 0xAA
	if err := a.Publish(ctx, 1, head1); err != nil {
		t.Fatalf("Publish(1): %v", err)
	}
	seq, head, err := a.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	if string(head) != string(head1) {
		t.Errorf("head = %x, want %x", head, head1)
	}

	head2 := make([]byte, 32)
	head2[0] = 0xBB
	if err := a.Publish(ctx, 2, head2); err != nil {
		t.Fatalf("Publish(2): %v", err)
	}
	seq, head, err = a.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if seq != 2 || string(head) != string(head2) {
		t.Errorf("Head after Publish(2) = (%d, %x), want (2, %x)", seq, head, head2)
	}
}

func TestLocalFileAnchor_IdempotentReplay(t *testing.T) {
	dir := t.TempDir()
	a := NewLocalFileAnchor(filepath.Join(dir, "anchor.txt"))
	ctx := context.Background()

	head1 := make([]byte, 32)
	head1[0] = 0xCC
	if err := a.Publish(ctx, 1, head1); err != nil {
		t.Fatalf("Publish(1): %v", err)
	}
	// Same seq, same bytes -- must succeed (idempotent).
	if err := a.Publish(ctx, 1, head1); err != nil {
		t.Errorf("idempotent replay: unexpected error: %v", err)
	}
}

func TestLocalFileAnchor_RejectsMismatchedReplay(t *testing.T) {
	dir := t.TempDir()
	a := NewLocalFileAnchor(filepath.Join(dir, "anchor.txt"))
	ctx := context.Background()

	head1 := make([]byte, 32)
	head1[0] = 0xDD
	if err := a.Publish(ctx, 1, head1); err != nil {
		t.Fatalf("Publish(1): %v", err)
	}

	head1Different := make([]byte, 32)
	head1Different[0] = 0xEE
	if err := a.Publish(ctx, 1, head1Different); err == nil {
		t.Error("expected error republishing seq 1 with a different head, got nil")
	}
}

func TestLocalFileAnchor_RejectsNonSequentialSeq(t *testing.T) {
	dir := t.TempDir()
	a := NewLocalFileAnchor(filepath.Join(dir, "anchor.txt"))
	ctx := context.Background()

	head := make([]byte, 32)
	// Skipping straight to seq 5 with nothing published yet must fail --
	// only seq 1 (curSeq+1 where curSeq=0) is accepted first.
	if err := a.Publish(ctx, 5, head); err == nil {
		t.Error("expected error for out-of-order seq, got nil")
	}
}
