package main

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/jkaninda/logger"
	"github.com/redis/go-redis/v9"
)

// Event is a server-to-client message pushed over SSE.
//
// Type selects the shape of the payload:
//   - "created"  → Entry is set (a new signature)
//   - "updated"  → Entry is set (pinned/hidden by a moderator)
//   - "deleted"  → ID is set (a removed signature)
//   - "reaction" → ID/Emoji/Count are set (an entry's tally changed)
//   - "settings" → Settings is set (signing paused, banner changed, theme forced)
//   - "presence" → Online/Replicas/Visitors are set (connected clients or active
//     visitor identities changed)
//   - "tick"     → Time/Version/Host are set (live server clock; new in v2),
//     plus Visitors (active identities; new in v3)
type Event struct {
	Type     string    `json:"type"`
	Entry    *Entry    `json:"entry,omitempty"`
	ID       uint      `json:"id,omitempty"`
	Emoji    string    `json:"emoji,omitempty"`
	Count    int       `json:"count,omitempty"`
	Settings *Settings `json:"settings,omitempty"`
	Online   int       `json:"online,omitempty"`
	Replicas int       `json:"replicas,omitempty"`
	Visitors int64     `json:"visitors,omitempty"`
	Time     string    `json:"time,omitempty"`
	Version  string    `json:"version,omitempty"`
	Host     string    `json:"host,omitempty"`
}

// Replica describes one running instance of the app and its SSE clients.
// Requests is the number of HTTP requests it has answered since boot: under
// load-balancing the counts diverge, and a rollout resets them one replica at
// a time — the rolling update made visible.
type Replica struct {
	Host     string    `json:"host"`
	Version  string    `json:"version"`
	Online   int       `json:"online"`
	Requests int64     `json:"requests"`
	Seen     time.Time `json:"seen"`
}

const (
	eventsChannel    = "guestbook:events"
	replicaKeyPrefix = "guestbook:replica:"
	replicaTTL       = 15 * time.Second
	heartbeatEvery   = 5 * time.Second
	syncEvent        = "sync"
)

// Broker fans events out to the SSE clients connected to this replica.
//
// Without Redis it is purely in-process, so a signature posted on replica A
// never reaches viewers on replica B. With Redis, published events go through
// a pub/sub channel that every replica listens to, and each replica advertises
// its client count under a TTL'd key so presence is cluster-wide.
type Broker struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}

	host, version string
	rdb           *redis.Client
	dirty         chan struct{}
	requests      func() int64
	// visitors reports how many visitor identities were recently active (from
	// the database, so it is cluster-wide by construction); may be nil.
	visitors func() int64

	cmu      sync.RWMutex
	replicas []Replica
	online   int
}

// NewBroker creates an in-process Broker. Call UseRedis to make it cluster-wide.
// requests reports how many HTTP requests this replica has served so far (may
// be nil); it is advertised with the presence heartbeat.
func NewBroker(host, version string, requests func() int64) *Broker {
	return &Broker{
		subs:     make(map[chan Event]struct{}),
		host:     host,
		version:  version,
		dirty:    make(chan struct{}, 1),
		requests: requests,
	}
}

// UseRedis switches the broker to Redis pub/sub. The client reconnects on its
// own, so an unreachable Redis at startup is not fatal.
func (b *Broker) UseRedis(url string) error {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return err
	}
	b.rdb = redis.NewClient(opts)
	return nil
}

// Mode reports "redis" when events are shared across replicas, else "local".
func (b *Broker) Mode() string {
	if b.rdb != nil {
		return "redis"
	}
	return "local"
}

// RedisStatus reports "disabled", "up" or "down".
func (b *Broker) RedisStatus(ctx context.Context) string {
	if b.rdb == nil {
		return "disabled"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := b.rdb.Ping(ctx).Err(); err != nil {
		return "down"
	}
	return "up"
}

// Run drives the Redis subscription and presence heartbeat until ctx is done.
// It is a no-op in local mode.
func (b *Broker) Run(ctx context.Context) {
	if b.rdb == nil {
		return
	}
	go b.listen(ctx)

	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	b.announce(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.dirty:
			b.announce(ctx, true)
		case <-t.C:
			b.announce(ctx, false)
		}
	}
}

// Close withdraws this replica from the presence set.
func (b *Broker) Close(ctx context.Context) {
	if b.rdb == nil {
		return
	}
	_ = b.rdb.Del(ctx, replicaKeyPrefix+b.host).Err()
	_ = b.publishRaw(ctx, Event{Type: syncEvent})
	_ = b.rdb.Close()
}

// Subscribe registers a new client and returns its event channel.
func (b *Broker) Subscribe() chan Event {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	b.presenceChanged()
	return ch
}

// Unsubscribe removes a client and closes its channel.
func (b *Broker) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; !ok {
		b.mu.Unlock()
		return
	}
	delete(b.subs, ch)
	close(ch)
	b.mu.Unlock()
	b.presenceChanged()
}

// Publish broadcasts an event to every client on every replica.
func (b *Broker) Publish(ev Event) {
	if b.rdb == nil {
		b.emit(ev)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.publishRaw(ctx, ev); err != nil {
		logger.Warn("redis publish failed, delivering locally only", "type", ev.Type, "error", err)
		b.emit(ev)
	}
}

// PublishLocal broadcasts an event to this replica's clients only.
func (b *Broker) PublishLocal(ev Event) { b.emit(ev) }

// Online returns the number of connected clients across all replicas.
func (b *Broker) Online() int {
	if b.rdb == nil {
		return b.localCount()
	}
	b.cmu.RLock()
	defer b.cmu.RUnlock()
	return b.online
}

// Replicas lists the running replicas and their client counts.
func (b *Broker) Replicas() []Replica {
	if b.rdb == nil {
		return []Replica{b.self()}
	}
	b.cmu.RLock()
	defer b.cmu.RUnlock()
	return append([]Replica(nil), b.replicas...)
}

func (b *Broker) self() Replica {
	var reqs int64
	if b.requests != nil {
		reqs = b.requests()
	}
	return Replica{Host: b.host, Version: b.version, Online: b.localCount(), Requests: reqs, Seen: time.Now().UTC()}
}

func (b *Broker) localCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

func (b *Broker) presenceChanged() {
	if b.rdb == nil {
		b.emit(Event{Type: "presence", Online: b.localCount(), Replicas: 1, Visitors: b.visitorsActive()})
		return
	}
	select {
	case b.dirty <- struct{}{}:
	default:
	}
}

// visitorsActive returns the recent-visitor count, or 0 when no reporter is
// registered.
func (b *Broker) visitorsActive() int64 {
	if b.visitors == nil {
		return 0
	}
	return b.visitors()
}

func (b *Broker) publishRaw(ctx context.Context, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return b.rdb.Publish(ctx, eventsChannel, payload).Err()
}

func (b *Broker) listen(ctx context.Context) {
	ps := b.rdb.Subscribe(ctx, eventsChannel)
	defer func() { _ = ps.Close() }()
	ch := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var ev Event
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
				continue
			}
			if ev.Type == syncEvent {
				b.refresh(ctx)
				continue
			}
			b.emit(ev)
		}
	}
}

// announce writes this replica's heartbeat and, when notify is set, tells the
// other replicas to recount presence.
func (b *Broker) announce(ctx context.Context, notify bool) {
	payload, _ := json.Marshal(b.self())
	if err := b.rdb.Set(ctx, replicaKeyPrefix+b.host, payload, replicaTTL).Err(); err != nil {
		logger.Warn("redis heartbeat failed", "error", err)
		return
	}
	if notify {
		_ = b.publishRaw(ctx, Event{Type: syncEvent})
	}
	b.refresh(ctx)
}

// refresh recomputes cluster presence from the replica keys and pushes a
// presence event to local clients when it changed.
func (b *Broker) refresh(ctx context.Context) {
	var keys []string
	iter := b.rdb.Scan(ctx, 0, replicaKeyPrefix+"*", 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if iter.Err() != nil || len(keys) == 0 {
		return
	}
	vals, err := b.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return
	}
	replicas := make([]Replica, 0, len(vals))
	total := 0
	for _, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue
		}
		var r Replica
		if json.Unmarshal([]byte(s), &r) == nil {
			replicas = append(replicas, r)
			total += r.Online
		}
	}
	sort.Slice(replicas, func(i, j int) bool { return replicas[i].Host < replicas[j].Host })

	b.cmu.Lock()
	changed := total != b.online || len(replicas) != len(b.replicas)
	b.online, b.replicas = total, replicas
	b.cmu.Unlock()

	if changed {
		b.emit(Event{Type: "presence", Online: total, Replicas: len(replicas), Visitors: b.visitorsActive()})
	}
}

// emit fans an event out to local subscribers. Sends are non-blocking: a slow
// client that has filled its buffer misses the event rather than stalling
// the broker.
func (b *Broker) emit(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
