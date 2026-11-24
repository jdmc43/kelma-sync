package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestMemoryDeletePrefixRemovesOnlyMatchingObjects(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	for _, key := range []string{"account-a/one", "account-a/two", "account-b/one"} {
		if err := store.Put(ctx, key, bytes.NewReader([]byte(key)), int64(len(key)), "text/plain"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := store.DeletePrefix(ctx, "account-a/"); err != nil {
		t.Fatalf("delete prefix: %v", err)
	}
	for _, key := range []string{"account-a/one", "account-a/two"} {
		if body, _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			if body != nil {
				body.Close()
			}
			t.Fatalf("matching object %s remains: %v", key, err)
		}
	}
	body, _, err := store.Get(ctx, "account-b/one")
	if err != nil {
		t.Fatalf("unrelated object was deleted: %v", err)
	}
	body.Close()
}

func TestFilesystemDeletePrefixRemovesAccountDirectoryOnly(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create filesystem store: %v", err)
	}
	ctx := context.Background()
	for _, key := range []string{"account-a/one", "account-a/nested/two", "account-b/one"} {
		if err := store.Put(ctx, key, bytes.NewReader([]byte(key)), int64(len(key)), "text/plain"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := store.DeletePrefix(ctx, "account-a/"); err != nil {
		t.Fatalf("delete prefix: %v", err)
	}
	if body, _, err := store.Get(ctx, "account-a/nested/two"); !errors.Is(err, ErrNotFound) {
		if body != nil {
			body.Close()
		}
		t.Fatalf("matching filesystem object remains: %v", err)
	}
	body, _, err := store.Get(ctx, "account-b/one")
	if err != nil {
		t.Fatalf("unrelated filesystem object was deleted: %v", err)
	}
	body.Close()
}

func TestDeletePrefixRejectsEmptyPrefix(t *testing.T) {
	if err := NewMemory().DeletePrefix(context.Background(), ""); err == nil {
		t.Fatal("empty prefix deletion succeeded")
	}
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create filesystem store: %v", err)
	}
	if err := store.DeletePrefix(context.Background(), ""); err == nil {
		t.Fatal("empty filesystem prefix deletion succeeded")
	}
}
