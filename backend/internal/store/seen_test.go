package store

import "testing"

func TestMarkSeenIdempotent(t *testing.T) {
	repo, _ := newTestRepo(t)

	if err := repo.MarkSeen(ctx, "src", "hash1"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	// A second mark for the same key must be a no-op (INSERT OR IGNORE), never
	// erroring and never adding a second row.
	if err := repo.MarkSeen(ctx, "src", "hash1"); err != nil {
		t.Fatalf("MarkSeen (2nd): %v", err)
	}

	if n := rawCount(t, repo, `SELECT count(*) FROM seen_hashes WHERE source_site='src' AND info_hash='hash1'`); n != 1 {
		t.Fatalf("seen_hashes rows = %d, want 1", n)
	}
}

func TestHasSeen(t *testing.T) {
	repo, _ := newTestRepo(t)

	seen, err := repo.HasSeen(ctx, "src", "missing")
	if err != nil {
		t.Fatalf("HasSeen (missing): %v", err)
	}
	if seen {
		t.Fatal("HasSeen returned true for an absent hash")
	}

	if err := repo.MarkSeen(ctx, "src", "hash1"); err != nil {
		t.Fatal(err)
	}

	seen, err = repo.HasSeen(ctx, "src", "hash1")
	if err != nil {
		t.Fatalf("HasSeen (present): %v", err)
	}
	if !seen {
		t.Fatal("HasSeen returned false for a tombstoned hash")
	}

	// The dedup key is (source_site, info_hash): the same hash under a different
	// site is a distinct key and must not be conflated.
	seen, err = repo.HasSeen(ctx, "other", "hash1")
	if err != nil {
		t.Fatalf("HasSeen (other site): %v", err)
	}
	if seen {
		t.Fatal("HasSeen conflated different source_site values")
	}
}
