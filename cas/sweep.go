package cas

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/zeebo/blake3"

	"github.com/progrium/h9reg/store"
)

// DefaultRepackBelow is the share of a pack's bytes still in use below
// which a sweep rewrites the pack.
const DefaultRepackBelow = 0.5

// SweepStats is what a sweep did, or would do in a dry run.
type SweepStats struct {
	Packs       int   // packs before the sweep
	Deleted     int   // packs with no live chunk, deleted
	Repacked    int   // packs rewritten without their dead chunks
	NewPacks    int   // packs written by repacking
	Loose       int   // loose chunks before the sweep
	LooseFreed  int   // loose chunks deleted
	FreedBytes  int64 // bytes of packs deleted, less new packs
	LiveChunks  int   // distinct live chunks found in packs
	DeadEntries int   // pack entries dropped
}

type packInfo struct {
	id       string
	entries  []packEntry
	size     int64 // bytes, from the index
	live     int64 // bytes of live entries
	liveN    int
	decision int
}

const (
	keepPack = iota
	deletePack
	repackPack
)

// SweepPacks deletes what live says is garbage: packs with no live chunk,
// and loose chunks. A pack whose live bytes are under repackBelow of it
// is rewritten: its live chunks, unless a kept pack also has them, go
// into new packs, and then it's deleted. Each index is deleted before its
// pack, so an index never names a missing pack for long.
//
// The caller must make sure nothing writes chunks meanwhile (package gc's
// lock), and tell running instances to reload their indexes after
// (ReloadPacks), since theirs may name deleted packs.
func SweepPacks(ctx context.Context, st store.Store, live func(id string) bool, repackBelow float64, target int64, dryRun bool, log *slog.Logger) (SweepStats, error) {
	var stats SweepStats
	if log == nil {
		log = slog.Default()
	}
	if target <= 0 {
		target = DefaultPackTarget
	}
	keys, err := st.List(ctx, "packidx/")
	if err != nil {
		return stats, err
	}
	packs := make([]*packInfo, len(keys))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 16)
	for i, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			id := strings.TrimPrefix(k, "packidx/")
			b, err := st.Get(ctx, k)
			var entries []packEntry
			if err == nil {
				entries, err = decodePackIndex(b)
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("pack %s: %w", id, err)
				}
				mu.Unlock()
				return
			}
			p := &packInfo{id: id, entries: entries}
			for _, e := range entries {
				p.size = max(p.size, e.off+e.n)
				if live(e.id) {
					p.live += e.n
					p.liveN++
				}
			}
			packs[i] = p
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return stats, firstErr
	}
	stats.Packs = len(packs)
	sort.Slice(packs, func(i, j int) bool { return packs[i].id < packs[j].id })

	// Decide each pack's fate, then which live chunks kept packs cover.
	covered := map[string]bool{}
	for _, p := range packs {
		switch {
		case p.liveN == 0:
			p.decision = deletePack
		case float64(p.live) < repackBelow*float64(p.size):
			p.decision = repackPack
		default:
			p.decision = keepPack
			for _, e := range p.entries {
				if live(e.id) {
					covered[e.id] = true
				}
			}
		}
	}

	// Copy the live chunks out of packs being rewritten.
	var out bytes.Buffer
	var outEntries []packEntry
	flush := func() error {
		if len(outEntries) == 0 {
			return nil
		}
		data := out.Bytes()
		sum := blake3.Sum256(data)
		id := hex.EncodeToString(sum[:])
		stats.NewPacks++
		stats.FreedBytes -= int64(len(data))
		if !dryRun {
			idx, err := encodePackIndex(outEntries)
			if err != nil {
				return err
			}
			if err := st.Put(ctx, PackKey(id), data); err != nil {
				return err
			}
			if err := st.Put(ctx, PackIndexKey(id), idx); err != nil {
				return err
			}
		}
		log.Debug("wrote pack", "pack", id, "chunks", len(outEntries), "bytes", len(data))
		out.Reset()
		outEntries = nil
		return nil
	}
	for _, p := range packs {
		if p.decision != repackPack {
			continue
		}
		data, err := st.Get(ctx, PackKey(p.id))
		if err != nil {
			return stats, fmt.Errorf("pack %s: %w", p.id, err)
		}
		for _, e := range p.entries {
			if !live(e.id) || covered[e.id] {
				continue
			}
			if e.off+e.n > int64(len(data)) {
				return stats, fmt.Errorf("pack %s: chunk %s past the end", p.id, e.id)
			}
			outEntries = append(outEntries, packEntry{id: e.id, off: int64(out.Len()), n: e.n, size: e.size})
			out.Write(data[e.off : e.off+e.n])
			covered[e.id] = true
			if int64(out.Len()) >= target {
				if err := flush(); err != nil {
					return stats, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	stats.LiveChunks = len(covered)

	// Every live chunk is now in a kept or new pack, so the rest can go.
	for _, p := range packs {
		if p.decision == keepPack {
			continue
		}
		if p.decision == deletePack {
			stats.Deleted++
		} else {
			stats.Repacked++
		}
		stats.DeadEntries += len(p.entries) - p.liveN
		stats.FreedBytes += p.size
		if dryRun {
			continue
		}
		for _, k := range []string{PackIndexKey(p.id), PackKey(p.id)} {
			if err := st.Delete(ctx, k); err != nil && !errors.Is(err, store.ErrNotFound) {
				return stats, err
			}
		}
	}

	// Loose chunks, from stores made before packs.
	loose, err := st.List(ctx, "chunks/")
	if err != nil {
		return stats, err
	}
	stats.Loose = len(loose)
	for _, k := range loose {
		id := strings.ReplaceAll(strings.TrimPrefix(k, "chunks/"), "/", "")
		if live(id) {
			continue
		}
		stats.LooseFreed++
		if dryRun {
			continue
		}
		if err := st.Delete(ctx, k); err != nil && !errors.Is(err, store.ErrNotFound) {
			return stats, err
		}
	}
	return stats, nil
}
