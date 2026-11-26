// Package storage is what the data directory holds and keeping it in
// shape: the report of the Storage page with its recommendations, the
// compact of the database, the daily self-check and the clean that runs
// when the disk is nearly full. kipp only reports a file it did not make,
// and never edits a job file: the recommendations say what to change.
package storage

import (
	"context"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/diskfree"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	// JobShare is the share of the database's bytes in use past which a
	// job's rows earn a recommendation to keep fewer builds.
	JobShare = 0.10
	// FreeShare is the share of the database file in free pages past
	// which a compact is recommended.
	FreeShare = 0.10
	// FloorBytes is the free-space floor of the data directory's disk, or
	// FloorShare of the disk when that is smaller, so that a small disk is
	// not always under it. Under the floor, Heal cleans what is safe to
	// clean.
	FloorBytes = 5 << 30
	// FloorShare is the floor of a disk too small for FloorBytes.
	FloorShare = 0.05
	// MeasureAge is how old the measure of the tables may be before a
	// read of the report starts a new one; it takes a pass over the file.
	MeasureAge = time.Hour
	// CompactWait is how long a compact waits for the builds to end, as a
	// safe restart does.
	CompactWait = 2 * time.Hour
)

// Counts is the server's quiet mode and what still runs or waits.
type Counts struct {
	Quiet           bool
	Running, Queued int
}

// Options are what a Service needs of the server around it.
type Options struct {
	Store     *store.Store
	Data      string
	Snapshot  func() *config.Snapshot
	SetQuiet  func(ctx context.Context, on bool) (Counts, error)
	Counts    func(ctx context.Context) (Counts, error)
	TrimCache func(ctx context.Context) (int, error)
	Space     func(dir string) (free, total uint64, ok bool)
	SameDisk  func(a, b string) bool
	TempDir   string
	Poll      time.Duration
	Go        func(f func())
	Now       func() time.Time
	Log       *slog.Logger
}

// Service is the storage of one server.
type Service struct {
	o Options

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	stopped    bool
	objects    store.DBObjects
	measuredAt time.Time
	measuring  bool
	measureErr string
	compact    CompactState
	check      *Check
	heal       *Heal
	changes    chan struct{}
}

// New makes a Service and reads the last self-check from the data
// directory.
func New(o Options) *Service {
	if o.Space == nil {
		o.Space = diskfree.Space
	}
	if o.SameDisk == nil {
		o.SameDisk = diskfree.SameDisk
	}
	if o.TempDir == "" {
		o.TempDir = os.Getenv("SQLITE_TMPDIR")
	}
	if o.TempDir == "" {
		o.TempDir = os.TempDir()
	}
	if o.Poll <= 0 {
		o.Poll = 2 * time.Second
	}
	if o.Go == nil {
		o.Go = func(f func()) { go f() }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	s := &Service{o: o, changes: make(chan struct{}), compact: CompactState{State: CompactIdle}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.check = readCheck(o.Data)
	return s
}

// Stop cancels the work the Service runs apart, a measure or a compact, and
// returns once it has ended; it starts none after. A compact is cancelled
// rather than waited for: it may wait hours for builds, and SQLite rolls
// back a VACUUM it interrupts.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

func (s *Service) apart(f func(ctx context.Context)) {
	s.o.Go(func() {
		defer s.wg.Done()
		f(s.ctx)
	})
}

// Changes is closed at the next change of the compact.
func (s *Service) Changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changes
}

func (s *Service) changedLocked() {
	close(s.changes)
	s.changes = make(chan struct{})
}

// Measure measures the tables and indexes of the database now.
func (s *Service) Measure(ctx context.Context) error {
	objs, err := s.o.Store.MeasureObjects(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.measuring = false
	if err != nil {
		s.measureErr = err.Error()
		s.o.Log.Error("measuring the database's tables", "err", err)
		return err
	}
	s.objects, s.measuredAt, s.measureErr = objs, s.o.Now(), ""
	return nil
}

// MeasureSoon starts a measure apart unless one runs or the last is
// younger than MeasureAge.
func (s *Service) MeasureSoon() {
	s.mu.Lock()
	if s.stopped || s.measuring || (!s.measuredAt.IsZero() && s.o.Now().Sub(s.measuredAt) < MeasureAge) {
		s.mu.Unlock()
		return
	}
	s.measuring = true
	s.wg.Add(1)
	s.mu.Unlock()
	s.apart(func(ctx context.Context) { _ = s.Measure(ctx) })
}

func floor(total int64) int64 {
	return min(FloorBytes, int64(float64(total)*FloorShare))
}

func (s *Service) space(dir string) (free, total int64, ok bool) {
	f, t, ok := s.o.Space(dir)
	return clampBytes(f), clampBytes(t), ok
}

func clampBytes(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
