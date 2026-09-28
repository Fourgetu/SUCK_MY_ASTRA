package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	observationsFileName	= "observations.json"

	observationsVersion	= 2

	observationsRecentMax	= 100

	observationsBucketMax	= 256

	observationsHourlyMax	= 48

	observationLearnMin	= 2

	observationLensMax	= 8
)

var observationsFlushInterval = 60 * time.Second

const (
	observationNormal	= "normal"
	observationLimited	= "limited"
	observationSilent	= "silent"
	observationOther	= "other"
)

type bucketObservation struct {
	AuthID	string	`json:"auth_id"`
	Model	string	`json:"model"`

	observationCounts

	LastKind	string	`json:"last_kind"`
	LastLen		int	`json:"last_len"`
	LastWrote	bool	`json:"last_wrote"`
	LastAt		string	`json:"last_at"`

	LastSignedKind	string	`json:"last_signed_kind,omitempty"`
	LastSignedAt	string	`json:"last_signed_at,omitempty"`
	LastSignedWrote	bool	`json:"last_signed_wrote,omitempty"`

	LastNaturalKind	string	`json:"last_natural_kind,omitempty"`
	LastNaturalAt	string	`json:"last_natural_at,omitempty"`

	Hourly	[]hourlyObservation	`json:"hourly,omitempty"`

	SignedLens	map[int]int	`json:"signed_lens,omitempty"`
}

type observationCounts struct {
	NaturalNormal	int64	`json:"natural_normal"`
	NaturalLimited	int64	`json:"natural_limited"`
	NaturalOther	int64	`json:"natural_other"`

	InjectedSilent	int64	`json:"injected_silent"`
	InjectedLimited	int64	`json:"injected_limited"`
	InjectedNormal	int64	`json:"injected_normal"`
	InjectedOther	int64	`json:"injected_other"`
}

func (c *observationCounts) add(wrote bool, kind string) {
	switch {
	case wrote && kind == observationSilent:
		c.InjectedSilent++
	case wrote && kind == observationLimited:
		c.InjectedLimited++
	case wrote && kind == observationNormal:
		c.InjectedNormal++
	case wrote:
		c.InjectedOther++
	case kind == observationNormal:
		c.NaturalNormal++
	case kind == observationLimited:
		c.NaturalLimited++
	default:
		c.NaturalOther++
	}
}

func (c *observationCounts) addAll(o observationCounts) {
	c.NaturalNormal += o.NaturalNormal
	c.NaturalLimited += o.NaturalLimited
	c.NaturalOther += o.NaturalOther
	c.InjectedSilent += o.InjectedSilent
	c.InjectedLimited += o.InjectedLimited
	c.InjectedNormal += o.InjectedNormal
	c.InjectedOther += o.InjectedOther
}

type hourlyObservation struct {
	Hour	string	`json:"hour"`
	observationCounts
}

func (b *bucketObservation) hourSlot(now time.Time) *observationCounts {
	hour := now.UTC().Truncate(time.Hour).Format(time.RFC3339)

	for i := len(b.Hourly) - 1; i >= 0; i-- {
		if b.Hourly[i].Hour == hour {
			return &b.Hourly[i].observationCounts
		}
	}

	b.Hourly = append(b.Hourly, hourlyObservation{Hour: hour})
	if len(b.Hourly) > observationsHourlyMax {
		b.Hourly = b.Hourly[len(b.Hourly)-observationsHourlyMax:]
	}
	return &b.Hourly[len(b.Hourly)-1].observationCounts
}

func (b bucketObservation) rollup(now time.Time, window time.Duration) observationCounts {
	cutoff := now.UTC().Add(-window)
	var out observationCounts
	for _, h := range b.Hourly {
		at, err := time.Parse(time.RFC3339, h.Hour)
		if err != nil || at.Before(cutoff) {
			continue
		}
		out.addAll(h.observationCounts)
	}
	return out
}

type observationSummary struct {
	observationCounts

	LastKind	string	`json:"last_kind"`
	LastLen		int	`json:"last_len"`
	LastWrote	bool	`json:"last_wrote"`
	LastAt		string	`json:"last_at"`

	LastSignedKind	string	`json:"last_signed_kind,omitempty"`
	LastSignedAt	string	`json:"last_signed_at,omitempty"`
	LastSignedWrote	bool	`json:"last_signed_wrote,omitempty"`

	LastNaturalKind	string	`json:"last_natural_kind,omitempty"`
	LastNaturalAt	string	`json:"last_natural_at,omitempty"`

	Recent24h	observationCounts	`json:"recent_24h"`
}

func (b bucketObservation) summary(now time.Time) observationSummary {
	return observationSummary{
		observationCounts:	b.observationCounts,
		LastKind:		b.LastKind,
		LastLen:		b.LastLen,
		LastWrote:		b.LastWrote,
		LastAt:			b.LastAt,
		LastSignedKind:		b.LastSignedKind,
		LastSignedAt:		b.LastSignedAt,
		LastSignedWrote:	b.LastSignedWrote,
		LastNaturalKind:	b.LastNaturalKind,
		LastNaturalAt:		b.LastNaturalAt,
		Recent24h:		b.rollup(now, 24*time.Hour),
	}
}

type observationEvent struct {
	At	string	`json:"at"`
	AuthID	string	`json:"auth_id"`
	Model	string	`json:"model"`
	Len	int	`json:"len"`
	Wrote	bool	`json:"wrote"`
	Kind	string	`json:"kind"`

	Served	string	`json:"served,omitempty"`
}

type observationSnapshot struct {
	Version		int			`json:"version"`
	Since		string			`json:"since"`
	UpdatedAt	string			`json:"updated_at"`
	Buckets		[]bucketObservation	`json:"buckets"`
	Recent		[]observationEvent	`json:"recent"`
}

var observations = struct {
	mu	sync.Mutex
	since	time.Time
	byKey	map[string]*bucketObservation
	recent	[]observationEvent
	dirty	bool
	lastOut	time.Time
	writing	bool
	dir	string
}{byKey: make(map[string]*bucketObservation)}

func classifyObservation(cfg pluginConfig, valueLen int) string {
	switch valueLen {
	case 0:
		return observationSilent
	case cfg.TemplateLength:
		return observationNormal
	case cfg.ReplaceLength:
		return observationLimited
	default:
		return observationOther
	}
}

func (b *bucketObservation) noteSignedLen(l int) bool {
	if b.SignedLens == nil {
		b.SignedLens = make(map[int]int, observationLensMax)
	}
	b.SignedLens[l]++
	if len(b.SignedLens) > observationLensMax {

		rarest, rarestN := 0, 0
		for cand, n := range b.SignedLens {
			if cand == l {
				continue
			}
			if rarestN == 0 || n < rarestN || (n == rarestN && cand < rarest) {
				rarest, rarestN = cand, n
			}
		}
		delete(b.SignedLens, rarest)
	}
	return b.SignedLens[l] >= observationLearnMin
}

func recordObservation(cfg pluginConfig, authID, model string, valueLen int, wrote bool) {
	recordEvent(authID, model, classifyObservation(cfg, valueLen), valueLen, wrote, "")
}

func recordDowngrade(authID, model, served string, tsLen int, wrote bool) {
	if strings.TrimSpace(served) == "" {
		return
	}
	recordEvent(authID, model, observationLimited, tsLen, wrote, served)
}

func recordEvent(authID, model, kind string, valueLen int, wrote bool, served string) {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {

		return
	}
	if kind == observationSilent && !wrote {

		return
	}

	now := time.Now()
	key := bucketKey(authID, model)

	observations.mu.Lock()
	cell := observations.byKey[key]
	if cell == nil {
		evictObservationBucketLocked()
		cell = &bucketObservation{AuthID: authID, Model: model}
		observations.byKey[key] = cell
	}
	if kind == observationOther && cell.noteSignedLen(valueLen) {

		kind = observationNormal
	}

	cell.observationCounts.add(wrote, kind)
	cell.hourSlot(now).add(wrote, kind)

	cell.LastKind = kind
	cell.LastLen = valueLen
	cell.LastWrote = wrote
	cell.LastAt = now.UTC().Format(time.RFC3339)

	if kind != observationSilent {
		cell.LastSignedKind = kind
		cell.LastSignedAt = cell.LastAt
		cell.LastSignedWrote = wrote
		if !wrote {
			cell.LastNaturalKind = kind
			cell.LastNaturalAt = cell.LastAt
		}
	}

	observations.recent = append(observations.recent, observationEvent{
		At:	cell.LastAt,
		AuthID:	authID,
		Model:	model,
		Len:	valueLen,
		Wrote:	wrote,
		Kind:	kind,
		Served:	served,
	})
	if len(observations.recent) > observationsRecentMax {
		observations.recent = observations.recent[len(observations.recent)-observationsRecentMax:]
	}
	observations.dirty = true
	dir := observations.dir
	due := now.Sub(observations.lastOut) >= observationsFlushInterval && !observations.writing
	if due {
		observations.writing = true
	}
	observations.mu.Unlock()

	if due && dir != "" {

		go flushObservations(dir)
	}
}

func evictObservationBucketLocked() {
	if len(observations.byKey) < observationsBucketMax {
		return
	}
	oldestKey, oldestAt := "", ""
	for key, cell := range observations.byKey {
		if oldestKey == "" || cell.LastAt < oldestAt {
			oldestKey, oldestAt = key, cell.LastAt
		}
	}
	if oldestKey != "" {
		delete(observations.byKey, oldestKey)
	}
}

func observationsSnapshot() ([]bucketObservation, []observationEvent, string) {
	observations.mu.Lock()
	defer observations.mu.Unlock()

	buckets := make([]bucketObservation, 0, len(observations.byKey))
	for _, cell := range observations.byKey {
		buckets = append(buckets, *cell)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].AuthID != buckets[j].AuthID {
			return buckets[i].AuthID < buckets[j].AuthID
		}
		return buckets[i].Model < buckets[j].Model
	})

	recent := make([]observationEvent, 0, len(observations.recent))
	for i := len(observations.recent) - 1; i >= 0; i-- {
		recent = append(recent, observations.recent[i])
	}

	since := ""
	if !observations.since.IsZero() {
		since = observations.since.UTC().Format(time.RFC3339)
	}
	return buckets, recent, since
}

func flushObservations(dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}

	observations.mu.Lock()
	if !observations.dirty {
		observations.writing = false
		observations.mu.Unlock()
		return
	}
	snap := observationSnapshot{
		Version:	observationsVersion,
		UpdatedAt:	time.Now().UTC().Format(time.RFC3339),
		Recent:		append([]observationEvent(nil), observations.recent...),
	}
	if !observations.since.IsZero() {
		snap.Since = observations.since.UTC().Format(time.RFC3339)
	}
	for _, cell := range observations.byKey {
		snap.Buckets = append(snap.Buckets, *cell)
	}
	observations.dirty = false
	observations.mu.Unlock()

	sort.Slice(snap.Buckets, func(i, j int) bool {
		if snap.Buckets[i].AuthID != snap.Buckets[j].AuthID {
			return snap.Buckets[i].AuthID < snap.Buckets[j].AuthID
		}
		return snap.Buckets[i].Model < snap.Buckets[j].Model
	})

	data, errMarshal := json.MarshalIndent(snap, "", "  ")
	if errMarshal == nil {
		errWrite := atomicWrite(filepath.Join(dir, observationsFileName), append(data, '\n'))
		if errWrite != nil {

			log.Printf(logPrefix+"could not write %s: %v", observationsFileName, errWrite)
			observations.mu.Lock()
			observations.dirty = true
			observations.mu.Unlock()
		}
	}

	observations.mu.Lock()
	observations.lastOut = time.Now()
	observations.writing = false
	observations.mu.Unlock()
}

func loadObservations(dir string) {
	dir = strings.TrimSpace(dir)

	observations.mu.Lock()
	defer observations.mu.Unlock()

	if observations.dir == dir && !observations.since.IsZero() {

		return
	}

	observations.dir = dir
	observations.byKey = make(map[string]*bucketObservation)
	observations.recent = nil
	observations.since = time.Now()
	observations.dirty = false

	observations.lastOut = time.Now()

	if dir == "" {
		return
	}

	raw, errRead := os.ReadFile(filepath.Join(dir, observationsFileName))
	if errRead != nil || len(raw) == 0 {

		return
	}
	var snap observationSnapshot
	if errUnmarshal := json.Unmarshal(raw, &snap); errUnmarshal != nil {
		log.Printf(logPrefix+"%s is unreadable, observation counts restart from empty: %v", observationsFileName, errUnmarshal)
		return
	}
	if snap.Version != observationsVersion {
		log.Printf(logPrefix+"%s is version %d, want %d; observation counts restart from empty",
			observationsFileName, snap.Version, observationsVersion)
		return
	}
	for i := range snap.Buckets {
		cell := snap.Buckets[i]
		if strings.TrimSpace(cell.AuthID) == "" || strings.TrimSpace(cell.Model) == "" {
			continue
		}
		observations.byKey[bucketKey(cell.AuthID, cell.Model)] = &cell
	}
	if len(snap.Recent) > observationsRecentMax {
		snap.Recent = snap.Recent[len(snap.Recent)-observationsRecentMax:]
	}
	observations.recent = snap.Recent
	if parsed, errParse := time.Parse(time.RFC3339, snap.Since); errParse == nil && !parsed.IsZero() {
		observations.since = parsed
	}
}

func flushObservationsNow() {
	observations.mu.Lock()
	dir := observations.dir
	observations.writing = true
	observations.mu.Unlock()
	flushObservations(dir)
}

func deleteObservation(authID, model string) bool {
	key := bucketKey(authID, model)
	observations.mu.Lock()
	defer observations.mu.Unlock()
	if _, ok := observations.byKey[key]; !ok {
		return false
	}
	delete(observations.byKey, key)
	observations.recent = dropObservationEventsLocked(observations.recent, key)
	observations.dirty = true
	return true
}

func clearAllObservations() {
	observations.mu.Lock()
	defer observations.mu.Unlock()
	observations.byKey = make(map[string]*bucketObservation)
	observations.recent = nil
	observations.dirty = true
}

func dropObservationEventsLocked(events []observationEvent, key string) []observationEvent {
	out := events[:0]
	for _, e := range events {
		if bucketKey(e.AuthID, e.Model) == key {
			continue
		}
		out = append(out, e)
	}
	return out
}
