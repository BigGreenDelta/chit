package store

import (
	"fmt"

	"ledger/internal/cache"
	"ledger/internal/gitx"
)

// This file is store's adapter onto internal/cache: the ref names, the two
// ref-writing primitives the cache needs, and the from-root fold it falls
// back to. The mechanism itself lives in internal/cache, which knows
// nothing about Store - that is what keeps the import in one direction.

// UpdateRefForce points a ref at an object unconditionally. Used only for
// refs/ledger-cache/*: a cache ref's history is meaningless, so every
// update is a replacement, and update-ref with no old value is exactly
// that. Every other ref this tool writes goes through CAS.
func (s Store) UpdateRefForce(name, sha string) error {
	_, stderr, code := s.Repo.Git("", "update-ref", name, sha)
	if code != 0 {
		return fmt.Errorf("git_failed: update-ref %s: %s", name, stderr)
	}
	return nil
}

// DeleteRef removes a ref, tolerating its absence - `cache reset` drops the
// ref before rebuilding and must not care whether there was one.
func (s Store) DeleteRef(name string) error {
	if _, ok := s.RevParse(name); !ok {
		return nil
	}
	_, stderr, code := s.Repo.Git("", "update-ref", "-d", name)
	if code != 0 {
		return fmt.Errorf("git_failed: update-ref -d %s: %s", name, stderr)
	}
	return nil
}

// Batch satisfies cache.Git; it is the same persistent cat-file child every
// other read in this process uses.
func (s Store) Batch(specs []string) ([]gitx.Object, error) { return s.Repo.Batch(specs) }

// WriteObject satisfies cache.Git.
func (s Store) WriteObject(typ string, payload []byte) (string, error) {
	return s.Repo.WriteObject(typ, payload)
}

// CacheSource binds one slug's fold cache to this store.
func (s Store) CacheSource(slug string) cache.Source {
	return cache.Source{
		Git: s, Slug: slug, LedgerRef: ref(slug), CacheRef: cacheRef(slug),
		Fold: func(rev string) (cache.Projection, error) { return s.foldProjection(slug, rev) },
	}
}

// foldProjection is the from-root fold: the whole-chain read this cache
// exists to avoid, kept as the single fallback every validation failure
// degrades to. rev is the ledger's own ref on the ordinary path, and an
// arbitrary commit for `cache verify`'s comparison against a blob's base.
func (s Store) foldProjection(slug, rev string) (cache.Projection, error) {
	evs, meta, d, err := s.eventsDAG(rev, slug)
	if err != nil {
		return cache.Projection{}, err
	}
	head, ok := s.RevParse(rev)
	if !ok {
		return cache.Projection{}, fmt.Errorf("%w: %s", ErrUnknownLedger, slug)
	}
	return cache.Project(slug, head, evs, meta, d), nil
}

// Projection answers a `ready`-shaped read through the fold cache, falling
// back to the whole-chain fold whenever the cache cannot be proven to
// describe this history. See cache.Source.Read for the three branches.
func (s Store) Projection(slug string) (cache.Projection, error) {
	return s.CacheSource(slug).Read()
}

// refreshCacheAfterWrite is the write path's cache hook. It runs after an
// append has already landed, and every error it produces is discarded on
// purpose: a cache that cannot be written must never turn a successful
// write into a failure. The cadence gate is inside MaybeRefresh.
func (s Store) refreshCacheAfterWrite(slug string) {
	_ = s.CacheSource(slug).MaybeRefresh()
}
