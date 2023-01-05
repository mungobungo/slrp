package pool

import (
	"context"
	"fmt"
	"golang.org/x/exp/rand"
	"gonum.org/v1/gonum/stat/distuv"
	"net/http"
	"sort"
	"time"

	"github.com/nfx/slrp/app"
	"github.com/nfx/slrp/pmux"

	"github.com/corpix/uarand"
)

type incoming struct {
	ctx   context.Context
	Proxy pmux.Proxy
	Speed time.Duration
}

type request struct {
	in      *http.Request
	out     chan *http.Response
	start   time.Time
	attempt int
	serial  int
}

type reply struct {
	r        request
	response *http.Response
	start    time.Time
	e        *entry
	err      error
}

type work struct {
	r     request
	e     *entry
	reply chan reply
}

type removal struct {
	proxy pmux.Proxy
	reply chan bool
}

type shard struct {
	parent       *Pool
	id           string
	Entries      []entry
	incoming     chan incoming
	remove       chan removal
	requests     chan request
	snapshot     chan chan []entry
	reanimate    chan bool
	reply        chan reply
	work         chan work //todo channel in pool
	minute       *time.Ticker
	tenSec       *time.Ticker
	ticks        int
	randomSource *rand.Rand
}

func (pool *shard) init(work chan work) {

	pool.work = work
	pool.incoming = make(chan incoming)
	pool.remove = make(chan removal)
	pool.requests = make(chan request)
	pool.reanimate = make(chan bool)
	pool.snapshot = make(chan chan []entry)
	pool.reply = make(chan reply)
	pool.minute = time.NewTicker(1 * time.Minute)
	pool.tenSec = time.NewTicker(10 * time.Second)
	pool.randomSource = rand.New(rand.NewSource(uint64(now().Unix())))

}

func (pool *shard) main(ctx app.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case res := <-pool.snapshot:
			snapshot := make([]entry, len(pool.Entries))
			for i := range pool.Entries {
				snapshot[i] = pool.Entries[i]
			}
			res <- snapshot
		case <-pool.tenSec.C:
			var av float64
			for _, e := range pool.Entries {
				av += float64(e.SuccessRate())
			}
			pool.parent.averageSuccessRateGauge.WithLabelValues(pool.id).Set(av / float64(len(pool.Entries)))
			var nt float64
			sampleSize := len(pool.Entries) / 10
			for i := 0; i < sampleSize; i++ {
				_, _, e := pool.sampleProxy()
				nt += float64(e.SuccessRate())
			}
			nt = nt / float64(sampleSize)
			pool.parent.successRateGauge.WithLabelValues(pool.id).Set(nt)

		case <-pool.minute.C:
			if pool.handleReanimate() {
				ctx.Heartbeat()
			}

		case <-pool.reanimate:
			pool.forceReanimate()
		case v := <-pool.remove:

			pool.removeProxy(v)
			ctx.Heartbeat()
		case v := <-pool.incoming:

			pool.add(v)

			ctx.Heartbeat()
		case r := <-pool.requests:
			pool.handleRequest(r)
		case r := <-pool.reply:
			pool.handleReply(r)
		}
	}
}

func (pool *shard) handleReanimate() bool {
	heartbeat := false
	for i := range pool.Entries {
		if pool.Entries[i].ReanimateIfNeeded() {
			heartbeat = true
		}
	}
	return heartbeat
}

func (pool *shard) forceReanimate() {
	for i := range pool.Entries {
		pool.Entries[i].ForceReanimate()
	}
}

func (pool *shard) removeProxy(r removal) {
	newEntries := []entry{}
	found := false
	for _, v := range pool.Entries {
		if v.Proxy == r.proxy {
			found = true
			continue
		}
		newEntries = append(newEntries, v)
	}
	pool.Entries = newEntries
	if found {
		log := app.Log.From(context.TODO())
		log.Info().Stringer("proxy", r.proxy).Msg("removed")

	}
	r.reply <- found
}

func (pool *shard) add(v incoming) {
	now := time.Now()

	pool.Entries = append(pool.Entries, entry{
		Proxy:     v.Proxy,
		FirstSeen: now.Unix(),
		LastSeen:  now.Unix(),
		Speed:     v.Speed,
		Seen:      1,
		Offered:   1,
		Ok:        true,
	})
	sort.Slice(pool.Entries, func(i, j int) bool {
		return pool.Entries[i].Speed < pool.Entries[j].Speed
	})
	log := app.Log.From(v.ctx)
	log.Info().Stringer("proxy", v.Proxy).Dur("speed", v.Speed).Msg("added")

}

func (pool *shard) firstAvailableProxy(r request) *entry {
	size := len(pool.Entries)
	if size == 0 {
		return nil
	}
	// offset := 0
	// defaultSorting(pool.Entries)

	offset := rand.Intn(size)
	// offset := r.serial % len(pool.Entries)

	// TODO: per-request offset selection strategy -
	// scrapes are encouraged to refresh the old or
	// the least offered proxies, but relays need fresher pool
	available := pool.Entries[offset:size]
	for idx := range available {
		e := &pool.Entries[offset+idx]
		if e.ConsiderSkip(r.in.Context()) {
			continue
		}
		return e
	}
	return nil
}

// ThompsonBandit if implemented correctly should be close to optimal
// https://auai.org/uai2016/proceedings/papers/20.pdf
func (pool *shard) ThompsonBandit() *entry {

	//ctx = app.Log.WithStringer(ctx, "proxy", entry.Proxy)
	size := len(pool.Entries)
	if size == 0 {
		return nil
	}
	if size == 1 {
		return &pool.Entries[0]
	}

	var nt float64

	for _, e := range pool.Entries {
		nt += float64(e.SuccessRate())
	}
	weights, bandi, b := pool.sampleProxy()
	pool.parent.successRateClick.WithLabelValues(pool.id).Set(weights[bandi])
	b.Offered += 1
	fmt.Printf("pool %v\t%v\t(%v/%v) \t%v \t %v \t%v \t%.4f\t SR:%.4f\n", pool.id, b.Proxy.IP(), bandi, size, weights[bandi], b.Offered, b.Succeed, float64(b.SuccessRate()/100), float64(nt)/float64(size))
	//pool.successRateGauge.Set(nt / float64(size))
	return b
}

func (pool *shard) sampleProxy() ([]float64, int, *entry) {
	mv := float64(-1)
	weights := make([]float64, len(pool.Entries))
	bandi := 0
	const scaleFactor = 1
	for idx, e := range pool.Entries {
		//failures
		b := e.Offered - e.Succeed
		//successes
		a := e.Succeed
		// we use punish factor to penalize the failed attempts more
		// allows to move dead proxies out of equation faster
		bet := distuv.Beta{Alpha: float64(a*scaleFactor + 1), Beta: float64(b + 1), Src: pool.randomSource}
		weights[idx] = bet.Rand()
		if weights[idx] > mv {
			bandi = idx
			mv = weights[idx]
		}

	}

	b := &pool.Entries[bandi]
	return weights, bandi, b
}

func (pool *shard) handleRequest(r request) {
	// log := app.Log.From(r.in.Context())
	r.in.Header.Set("User-Agent", uarand.GetRandom())
	var entry *entry
	pool.parent.proxyCountGauge.WithLabelValues(pool.id).Set(float64(len(pool.Entries)))
	if len(pool.Entries) < 10 {
		entry = pool.firstAvailableProxy(r)
	} else {
		entry = pool.ThompsonBandit()
	}
	pool.ticks += 1
	if entry == nil {
		// this pool has no entries, try next one
		headers := http.Header{}
		headers.Add("X-Proxy-Serial", fmt.Sprintf("%d", r.serial))
		r.out <- &http.Response{
			StatusCode: 552,
			Status:     "Proxy Pool Exhausted",
			Header:     headers,
			Request:    r.in,
		}
		return
	}

	// log.Debug().
	// 	Stringer("url", req.URL).
	// 	Stringer("proxy", entry.Proxy).
	// 	Dur("t", time.Since(start)).
	// 	Msg("prepare request")
	ctx := entry.Proxy.InContext(r.in.Context())
	ctx = app.Log.WithStringer(ctx, "proxy", entry.Proxy)
	ctx = app.Log.WithStr(ctx, "serial", fmt.Sprint(r.serial))
	r.in = r.in.WithContext(ctx)
	go func() {
		// send work via goroutine, so we're not deadlocked by work trying to send to handleReply
		// this leaks a goroutine to certain extent, but hopefully with throttling on serial channel
		// we can overcome this difficulty
		select {
		case <-ctx.Done():
			return
		case pool.work <- work{
			reply: pool.reply,
			e:     entry,
			r:     r,
		}: // bada boom ts
		}
	}()
}

func (pool *shard) handleReply(r reply) {
	request := r.r
	res := r.response
	log := app.Log.From(r.r.in.Context())
	err := r.err
	entry := r.e
	if err == nil && res.StatusCode >= 400 {
		err = fmt.Errorf(res.Status)
	}
	if err == nil {
		entry.MarkSuccess()
		res.Header.Set("X-Proxy-Through", entry.Proxy.String())
		res.Header.Set("X-Proxy-Attempt", fmt.Sprint(request.attempt))
		res.Header.Set("X-Proxy-Offered", fmt.Sprint(entry.Offered))
		res.Header.Set("X-Proxy-Succeed", fmt.Sprint(entry.Succeed))
		res.Header.Set("X-Proxy-Serial", fmt.Sprint(request.serial))
		log.Debug().
			Stringer("t", time.Since(request.start)).
			Int("offered", entry.Offered).
			Msg("forwarded")
		r.r.out <- res
		return
	}
	// TODO: if more than 10 failed offers in this hour, mark dead till beginning of next hour
	entry.MarkFailure(err)
	log.Debug().
		Err(app.ShErr(err)).
		Int("timeouts", entry.Timeouts).
		Int("offered", entry.Offered).
		Stringer("t", time.Since(request.start)).
		Msg("forwarding failed")
	if request.attempt >= 10 {
		headers := http.Header{}
		headers.Add("X-Proxy-Serial", fmt.Sprintf("%d", request.serial))
		request.out <- &http.Response{
			StatusCode: 429,
			Status:     err.Error(),
			Header:     headers,
			Request:    request.in,
		}
		return
	}
	// todo: proxies becoming dead (connections rejected, etc)
	request.out <- nil
}
