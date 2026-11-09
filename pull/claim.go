package pull

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/progrium/h9reg/store"
)

// Claims keep instances sharing a store from pulling the same image at
// once. Before it pulls, an instance creates pulling/<digest>/<platform>
// with PutIfAbsent. Instances that lose wait for the image record to
// appear. The winner rewrites its claim every ClaimTTL/4 while it works
// and deletes it when done, so a claim not rewritten for ClaimTTL belongs
// to an instance that died, and a waiter takes it over.
//
// A claim only saves work: everything a pull writes is content addressed
// or the same for every writer, so two instances pulling one image at
// once, after a takeover race, still store the same result.

// DefaultClaimTTL is how long a claim stands without being refreshed.
const DefaultClaimTTL = 2 * time.Minute

// Claim is the stored record of a pull in progress, under pulling/.
type Claim struct {
	Instance string    `json:"instance"`
	Time     time.Time `json:"time"` // when the claim was last refreshed
}

func claimKey(top v1.Hash, plat v1.Platform) string {
	return digestKey("pulling/", top) + "/" + strings.ReplaceAll(plat.String(), "/", "_")
}

// NewInstanceID returns a name for this process in claims: its host name
// and a random suffix.
func NewInstanceID() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "h9reg"
	}
	b := make([]byte, 4)
	rand.Read(b)
	return h + "-" + hex.EncodeToString(b)
}

func (p *Puller) claimTTL() time.Duration {
	if p.ClaimTTL > 0 {
		return p.ClaimTTL
	}
	return DefaultClaimTTL
}

func (p *Puller) pollInterval() time.Duration {
	if p.PollInterval > 0 {
		return p.PollInterval
	}
	return 500 * time.Millisecond
}

func (p *Puller) instance() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Instance == "" {
		p.Instance = NewInstanceID()
	}
	return p.Instance
}

// claimedPull pulls an image once across every instance sharing the
// store: it pulls under a claim, or waits for the instance holding one.
func (p *Puller) claimedPull(ctx context.Context, top v1.Hash, plat v1.Platform, pull func(context.Context) (*Image, error)) (*Image, error) {
	key := claimKey(top, plat)
	me := p.instance()
	ttl := p.claimTTL()
	waiting := false
	for {
		won, err := putClaim(ctx, p.Store, key, Claim{Instance: me, Time: time.Now().UTC()}, true)
		if err != nil {
			return nil, err
		}
		if won {
			return p.pullHolding(ctx, key, me, func(ctx context.Context) (*Image, error) {
				// Another instance may have finished since our lookup.
				if img, err := p.lookup(ctx, top, plat); img != nil || err != nil {
					return img, err
				}
				return pull(ctx)
			})
		}
		var c Claim
		found, err := getJSON(ctx, p.Store, key, &c)
		if err != nil {
			return nil, err
		}
		if !found {
			continue // released between our put and get
		}
		if age := time.Since(c.Time); age > ttl {
			p.log().Warn("taking over stale pull claim", "key", key, "holder", c.Instance, "age", age.Round(time.Second))
			if err := p.Store.Delete(ctx, key); err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			continue
		}
		if !waiting {
			p.log().Info("waiting for another instance's pull", "key", key, "holder", c.Instance)
			waiting = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.pollInterval()):
		}
		if img, err := p.lookup(ctx, top, plat); img != nil || err != nil {
			return img, err
		}
	}
}

// pullHolding runs pull while refreshing the claim, then releases it.
func (p *Puller) pullHolding(ctx context.Context, key, me string, pull func(context.Context) (*Image, error)) (*Image, error) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(p.claimTTL() / 4)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if _, err := putClaim(ctx, p.Store, key, Claim{Instance: me, Time: time.Now().UTC()}, false); err != nil {
					p.log().Warn("refresh pull claim", "key", key, "err", err)
				}
			}
		}
	}()
	img, err := pull(ctx)
	close(stop)
	<-done
	if derr := p.Store.Delete(ctx, key); derr != nil && !errors.Is(derr, store.ErrNotFound) {
		p.log().Warn("release pull claim", "key", key, "err", derr)
	}
	return img, err
}

func putClaim(ctx context.Context, st store.Store, key string, c Claim, ifAbsent bool) (bool, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	if ifAbsent {
		return st.PutIfAbsent(ctx, key, b)
	}
	return true, st.Put(ctx, key, b)
}
