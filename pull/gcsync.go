package pull

import (
	"context"
	"strings"
	"time"
)

// Garbage collection (package gc) deletes chunks and trees no kept image
// uses. A pull can reuse any stored chunk or tree, so a pull and a
// collection must not overlap. They meet in two objects:
//
//   - gc/lock: a collection creates it before it marks, refreshes it like a
//     pull claim, and deletes it when done. A puller that finds it after
//     taking its claim releases the claim and waits.
//   - pulling/: a collection waits until no fresh claim is left before it
//     marks. Either the puller sees the lock or the collection sees the
//     claim.
//
// Instances keep pack indexes in memory. A collection that deleted or
// rewrote packs writes gc/epoch last, and a puller that finds a new epoch
// reloads its pack indexes before it pulls, so it never counts a chunk
// as stored because of a pack that's gone.
const (
	GCLockKey  = "gc/lock"
	GCEpochKey = "gc/epoch"
)

// GCLock is the stored record of a collection in progress, and of the
// last one, under gc/epoch.
type GCLock struct {
	Instance string    `json:"instance"`
	Time     time.Time `json:"time"`
}

// Use is the stored record of an image's last attach, under used/.
type Use struct {
	Time time.Time `json:"time"`
}

// UsedKey is where an image's last attach is recorded.
func UsedKey(manifest string) string { return "used/" + strings.Replace(manifest, ":", "/", 1) }

// touchEvery is how often an instance records an attach of one image.
// Collection keeps images by days, so an hour is plenty fine.
const touchEvery = time.Hour

// Touch records that an image was attached, at most once an hour per
// image per instance.
func (p *Puller) Touch(ctx context.Context, img *Image) {
	p.mu.Lock()
	if p.touched == nil {
		p.touched = map[string]time.Time{}
	}
	last := p.touched[img.Manifest]
	if time.Since(last) < touchEvery {
		p.mu.Unlock()
		return
	}
	p.touched[img.Manifest] = time.Now()
	p.mu.Unlock()
	if err := putJSON(ctx, p.Store, UsedKey(img.Manifest), Use{Time: time.Now().UTC()}); err != nil {
		p.log().Warn("recording attach", "manifest", img.Manifest, "err", err)
	}
}

// gcRunning reports whether a collection holds the lock: one refreshed
// within ClaimTTL.
func (p *Puller) gcRunning(ctx context.Context) (bool, error) {
	var l GCLock
	found, err := getJSON(ctx, p.Store, GCLockKey, &l)
	if err != nil || !found {
		return false, err
	}
	return time.Since(l.Time) < p.claimTTL(), nil
}

// syncEpoch reloads the pack indexes if a collection finished since they
// were last loaded whole.
func (p *Puller) syncEpoch(ctx context.Context) error {
	var e GCLock
	found, err := getJSON(ctx, p.Store, GCEpochKey, &e)
	if err != nil || !found {
		return err
	}
	if !p.CAS.PacksLoadedBefore(e.Time) {
		return nil
	}
	p.log().Info("reloading pack indexes after garbage collection", "collected", e.Time)
	return p.CAS.ReloadPacks(ctx)
}
