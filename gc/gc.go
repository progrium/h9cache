// Package gc deletes what no kept image needs.
//
// An image is kept while it was pulled or attached within Keep. A
// collection deletes the records of images that aren't kept, then marks
// every tree and chunk the kept images and their layers reach, and deletes
// the rest: unmarked trees, packs with no live chunk, and loose chunks.
// Packs that are mostly garbage are rewritten first. See package pull for
// how a collection and pulls stay out of each other's way.
package gc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/progrium/h9cache/cas"
	"github.com/progrium/h9cache/imagefs"
	"github.com/progrium/h9cache/pull"
	"github.com/progrium/h9cache/store"
	"github.com/progrium/h9cache/tree"
)

// DefaultKeep is how long an unused image is kept.
const DefaultKeep = 30 * 24 * time.Hour

// Options configure a collection.
type Options struct {
	Keep        time.Duration // DefaultKeep if zero
	RepackBelow float64       // cas.DefaultRepackBelow if zero
	PackTarget  int64         // size of packs written by repacking
	DryRun      bool          // report what would go, delete nothing
	Instance    string        // name in the lock; pull.NewInstanceID if empty
	ClaimTTL    time.Duration // as the pullers' ClaimTTL; pull.DefaultClaimTTL if zero
	Log         *slog.Logger
}

// Report is what a collection did.
type Report struct {
	Images, ImagesDeleted int
	Layers, LayersDeleted int
	Trees, TreesDeleted   int
	RefsDeleted           int
	OtherDeleted          int // platform, index, use, and mkroot records
	KeptAllLayers         bool
	Packs                 cas.SweepStats
	Took                  time.Duration
}

func (r Report) String() string {
	return fmt.Sprintf("images %d (%d deleted), layers %d (%d deleted), trees %d (%d deleted), refs %d deleted, other records %d deleted; "+
		"packs %d (%d deleted, %d rewritten into %d), loose chunks %d (%d deleted), %d MB freed; took %v",
		r.Images, r.ImagesDeleted, r.Layers, r.LayersDeleted, r.Trees, r.TreesDeleted, r.RefsDeleted, r.OtherDeleted,
		r.Packs.Packs, r.Packs.Deleted, r.Packs.Repacked, r.Packs.NewPacks, r.Packs.Loose, r.Packs.LooseFreed,
		r.Packs.FreedBytes>>20, r.Took.Round(time.Millisecond))
}

// ErrRunning is returned when another collection holds the lock.
var ErrRunning = errors.New("gc: another collection is running")

type collector struct {
	st   store.Store
	opt  Options
	log  *slog.Logger
	now  time.Time
	rep  Report
	keep time.Duration
}

// Run collects garbage in st.
func Run(ctx context.Context, st store.Store, opt Options) (Report, error) {
	c := &collector{st: st, opt: opt, log: opt.Log, now: time.Now(), keep: opt.Keep}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.keep <= 0 {
		c.keep = DefaultKeep
	}
	if c.opt.RepackBelow == 0 {
		c.opt.RepackBelow = cas.DefaultRepackBelow
	}
	if c.opt.ClaimTTL <= 0 {
		c.opt.ClaimTTL = pull.DefaultClaimTTL
	}
	if c.opt.Instance == "" {
		c.opt.Instance = pull.NewInstanceID()
	}
	if !opt.DryRun {
		release, err := c.lock(ctx)
		if err != nil {
			return c.rep, err
		}
		defer release()
		if err := c.waitForPulls(ctx); err != nil {
			return c.rep, err
		}
	}
	err := c.run(ctx)
	c.rep.Took = time.Since(c.now)
	return c.rep, err
}

func (c *collector) run(ctx context.Context) error {
	// Which images stay.
	images, err := c.st.List(ctx, "images/")
	if err != nil {
		return err
	}
	c.rep.Images = len(images)
	var kept []*pull.Image
	dead := map[string]bool{} // manifests
	for _, k := range images {
		var img pull.Image
		if err := c.getJSON(ctx, k, &img); err != nil {
			return err
		}
		last := img.Pulled
		var use pull.Use
		if err := c.getJSON(ctx, pull.UsedKey(img.Manifest), &use); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if use.Time.After(last) {
			last = use.Time
		}
		if c.now.Sub(last) < c.keep {
			kept = append(kept, &img)
			continue
		}
		dead[img.Manifest] = true
		c.rep.ImagesDeleted++
		c.log.Info("image unused", "manifest", img.Manifest, "last", last, "deleted", !c.opt.DryRun)
		// The record goes first, so nothing finds it pointing at trees
		// this collection deletes.
		if err := c.delete(ctx, k, imagefs.IndexKey(img.Manifest), pull.UsedKey(img.Manifest)); err != nil {
			return err
		}
	}

	// Records that only point at images.
	if err := c.sweepRecords(ctx, dead); err != nil {
		return err
	}

	// Which layer records stay. Records from before images listed their
	// layers can't say, so then every layer record stays.
	keptLayers := map[string]bool{}
	for _, img := range kept {
		if img.Layers == nil {
			c.rep.KeptAllLayers = true
		}
		for _, d := range img.Layers {
			keptLayers[d] = true
		}
	}
	if c.rep.KeptAllLayers {
		c.log.Info("an image record predates layer lists; keeping every layer record")
	}
	layers, err := c.st.List(ctx, "layers/")
	if err != nil {
		return err
	}
	c.rep.Layers = len(layers)
	roots := map[string]bool{}
	for _, img := range kept {
		roots[img.Tree] = true
	}
	for _, k := range layers {
		digest := strings.Replace(strings.TrimPrefix(k, "layers/"), "/", ":", 1)
		if c.rep.KeptAllLayers || keptLayers[digest] {
			var l pull.Layer
			if err := c.getJSON(ctx, k, &l); err != nil {
				return err
			}
			roots[l.Tree] = true
			continue
		}
		c.rep.LayersDeleted++
		if err := c.delete(ctx, k); err != nil {
			return err
		}
	}

	// Mark.
	trees := map[string]bool{}
	chunks := map[string]bool{}
	if err := c.mark(ctx, roots, trees, chunks); err != nil {
		return err
	}
	c.log.Info("marked", "images", len(kept), "trees", len(trees), "chunks", len(chunks))

	// Sweep trees, then chunks.
	keys, err := c.st.List(ctx, "trees/")
	if err != nil {
		return err
	}
	c.rep.Trees = len(keys)
	for _, k := range keys {
		id := strings.ReplaceAll(strings.TrimPrefix(k, "trees/"), "/", "")
		if trees[id] {
			continue
		}
		c.rep.TreesDeleted++
		if err := c.delete(ctx, k); err != nil {
			return err
		}
	}
	c.rep.Packs, err = cas.SweepPacks(ctx, c.st, func(id string) bool { return chunks[id] },
		c.opt.RepackBelow, c.opt.PackTarget, c.opt.DryRun, c.log)
	if err != nil {
		return err
	}
	if c.opt.DryRun {
		return nil
	}
	// Last: running instances reload their pack indexes before their next
	// pull.
	return c.putJSON(ctx, pull.GCEpochKey, pull.GCLock{Instance: c.opt.Instance, Time: time.Now().UTC()})
}

// sweepRecords deletes refs and mkroot lookups older than Keep, which are
// only remembered answers, and platform and use records of deleted or
// missing images.
func (c *collector) sweepRecords(ctx context.Context, dead map[string]bool) error {
	refs, err := c.st.List(ctx, "refs/")
	if err != nil {
		return err
	}
	for _, k := range refs {
		var r pull.Ref
		if err := c.getJSON(ctx, k, &r); err != nil {
			return err
		}
		if c.now.Sub(r.Resolved) >= c.keep {
			c.rep.RefsDeleted++
			if err := c.delete(ctx, k); err != nil {
				return err
			}
		}
	}
	plats, err := c.st.List(ctx, "platforms/")
	if err != nil {
		return err
	}
	for _, k := range plats {
		var m string
		if err := c.getJSON(ctx, k, &m); err != nil {
			return err
		}
		if dead[m] {
			c.rep.OtherDeleted++
			if err := c.delete(ctx, k); err != nil {
				return err
			}
		}
	}
	lookups, err := c.st.List(ctx, "mkroot/")
	if err != nil {
		return err
	}
	for _, k := range lookups {
		var l struct {
			Checked time.Time `json:"checked"`
		}
		if err := c.getJSON(ctx, k, &l); err != nil {
			return err
		}
		if c.now.Sub(l.Checked) >= c.keep {
			c.rep.OtherDeleted++
			if err := c.delete(ctx, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// mark walks every tree under roots, recording trees and file chunks.
func (c *collector) mark(ctx context.Context, roots, trees, chunks map[string]bool) error {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	var firstErr error
	var visit func(id string)
	visit = func(id string) {
		defer wg.Done()
		sem <- struct{}{}
		entries, err := tree.Read(ctx, c.st, id)
		<-sem
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		for _, e := range entries {
			switch e.Kind {
			case tree.Dir:
				if !trees[e.Tree] {
					trees[e.Tree] = true
					wg.Add(1)
					go visit(e.Tree)
				}
			case tree.File:
				for _, ch := range e.Chunks {
					chunks[ch.ID] = true
				}
			}
		}
	}
	var todo []string
	for id := range roots {
		if !trees[id] {
			trees[id] = true
			todo = append(todo, id)
		}
	}
	wg.Add(len(todo))
	for _, id := range todo {
		go visit(id)
	}
	wg.Wait()
	return firstErr
}

func (c *collector) getJSON(ctx context.Context, key string, v any) error {
	b, err := c.st.Get(ctx, key)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func (c *collector) putJSON(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.st.Put(ctx, key, b)
}

// delete deletes keys, unless this is a dry run. Missing keys are fine.
func (c *collector) delete(ctx context.Context, keys ...string) error {
	if c.opt.DryRun {
		return nil
	}
	for _, k := range keys {
		if err := c.st.Delete(ctx, k); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// lock takes gc/lock, taking over one not refreshed within ClaimTTL, and
// refreshes it until released.
func (c *collector) lock(ctx context.Context) (func(), error) {
	rec := func() []byte {
		b, _ := json.Marshal(pull.GCLock{Instance: c.opt.Instance, Time: time.Now().UTC()})
		return b
	}
	for {
		won, err := c.st.PutIfAbsent(ctx, pull.GCLockKey, rec())
		if err != nil {
			return nil, err
		}
		if won {
			break
		}
		var l pull.GCLock
		if err := c.getJSON(ctx, pull.GCLockKey, &l); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if time.Since(l.Time) < c.opt.ClaimTTL {
			return nil, fmt.Errorf("%w (%s)", ErrRunning, l.Instance)
		}
		c.log.Warn("taking over stale gc lock", "holder", l.Instance)
		if err := c.st.Delete(ctx, pull.GCLockKey); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(c.opt.ClaimTTL / 4)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := c.st.Put(ctx, pull.GCLockKey, rec()); err != nil {
					c.log.Warn("refresh gc lock", "err", err)
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		if err := c.st.Delete(context.WithoutCancel(ctx), pull.GCLockKey); err != nil && !errors.Is(err, store.ErrNotFound) {
			c.log.Warn("release gc lock", "err", err)
		}
	}, nil
}

// waitForPulls waits until no pull claim was refreshed within ClaimTTL.
func (c *collector) waitForPulls(ctx context.Context) error {
	logged := false
	for {
		keys, err := c.st.List(ctx, "pulling/")
		if err != nil {
			return err
		}
		busy := 0
		for _, k := range keys {
			var cl pull.Claim
			if err := c.getJSON(ctx, k, &cl); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				return err
			}
			if time.Since(cl.Time) < c.opt.ClaimTTL {
				busy++
			}
		}
		if busy == 0 {
			return nil
		}
		if !logged {
			c.log.Info("waiting for pulls to finish", "pulls", busy)
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
