package sync

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/aybabtme/goamz/s3"
	"github.com/bmizerany/perks/quantile"
	"github.com/dustin/go-humanize"
	"io"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	targetP50 = 0.50
	targetP95 = 0.95
)

var (
	q = struct{}{}

	// BufferFactor of decode/sync channels,
	// which are BufferFactor-times bigger than their
	// parallelism.
	BufferFactor = 10
)

// SyncerFunc syncs an s3.Key from a source to a destination bucket.
type SyncerFunc func(src *s3.Bucket, dst *s3.Bucket, key s3.Key) error

func defaultSyncer(src, dst *s3.Bucket, key s3.Key) error {
	_, err := dst.PutCopy(key.Key, s3.Private, s3.CopyOptions{}, src.Name+"/"+key.Key)
	return err
}

// NewSyncTask creates a sync task that will sync keys from src onto dst.
func NewSyncTask(el *log.Logger, src, dst *s3.Bucket) (*SyncTask, error) {

	// before starting the sync, make sure our s3 object is usable (credentials and such)
	_, err := src.List("/", "/", "/", 1)
	if err != nil {
		// if we can't list, we abort right away
		return nil, fmt.Errorf("couldn't list source bucket %q: %v", src.Name, err)
	}
	_, err = dst.List("/", "/", "/", 1)
	if err != nil {
		return nil, fmt.Errorf("couldn't list destination bucket %q: %v", dst.Name, err)
	}

	return &SyncTask{
		RetryBase:  time.Second,
		MaxRetry:   10,
		DecodePara: runtime.NumCPU(),
		SyncPara:   200,
		Sync:       defaultSyncer,

		elog:     el,
		src:      src,
		dst:      dst,
		qtStream: quantile.NewTargeted(targetP50, targetP95),
	}, nil
}

// SyncTask synchronizes keys between two buckets.
type SyncTask struct {
	RetryBase  time.Duration
	MaxRetry   int
	DecodePara int
	SyncPara   int
	Sync       SyncerFunc

	elog *log.Logger

	src *s3.Bucket
	dst *s3.Bucket

	qtStreamL sync.Mutex
	qtStream  *quantile.Stream

	// shared stats between goroutines, use sync/atomic
	fileLines   int64
	decodedKeys int64
	syncedKeys  int64
	inflight    int64
}

// Start the task, reading all the keys that need to be sync'd
// from the input reader, in JSON form, copying the keys in src onto dst.
func (s *SyncTask) Start(input io.Reader, synced, failed io.Writer) error {

	start := time.Now()

	ticker := time.NewTicker(time.Second)
	go s.printProgress(ticker)

	keysIn := make(chan s3.Key, s.SyncPara*BufferFactor)
	keysOk := make(chan s3.Key, s.SyncPara*BufferFactor)
	keysFail := make(chan s3.Key, s.SyncPara*BufferFactor)

	decoders := make(chan []byte, s.DecodePara*BufferFactor)

	// start JSON decoders
	log.Printf("starting %d key decoders, buffer size %d", s.DecodePara, cap(decoders))
	decGroup := sync.WaitGroup{}
	for i := 0; i < s.DecodePara; i++ {
		decGroup.Add(1)
		go s.decode(&decGroup, decoders, keysIn)
	}

	// start S3 sync workers
	log.Printf("starting %d key sync workers, buffer size %d", s.SyncPara, cap(keysIn))
	syncGroup := sync.WaitGroup{}
	for i := 0; i < s.SyncPara; i++ {
		syncGroup.Add(1)
		go s.syncKey(&syncGroup, s.src, s.dst, keysIn, keysOk, keysFail)
	}

	// track keys that have been sync'd, and those that we failed to sync.
	log.Printf("starting to write progress")
	encGroup := sync.WaitGroup{}
	encGroup.Add(2)
	go s.encode(&encGroup, synced, keysOk)
	go s.encode(&encGroup, failed, keysFail)

	// feed the pipeline by reading the listing file
	log.Printf("starting to read key listing file")
	err := s.readLines(input, decoders)

	// when done reading the source file, wait until the decoders
	// are done.
	log.Printf("%v: done reading %s lines",
		time.Since(start),
		humanize.Comma(atomic.LoadInt64(&s.fileLines)))
	close(decoders)
	decGroup.Wait()

	// when the decoders are all done, wait for the sync workers to finish
	log.Printf("%v: done decoding %s keys",
		time.Since(start),
		humanize.Comma(atomic.LoadInt64(&s.decodedKeys)))

	close(keysIn)
	syncGroup.Wait()

	close(keysOk)
	close(keysFail)

	encGroup.Wait()

	ticker.Stop()

	// the source file is read, all keys were decoded and sync'd. we're done.
	log.Printf("%v: done syncing %s keys",
		time.Since(start),
		humanize.Comma(atomic.LoadInt64(&s.syncedKeys)))

	return err
}

// prints progress and stats as we go, handy to figure out what's going on
// and how the tool performs.
func (s *SyncTask) printProgress(tick *time.Ticker) {
	for _ = range tick.C {
		s.qtStreamL.Lock()
		p50, p95 := s.qtStream.Query(targetP50), s.qtStream.Query(targetP95)
		s.qtStream.Reset()
		s.qtStreamL.Unlock()

		log.Printf("fileLines=%s\tdecodedKeys=%s\tsyncedKeys=%s\tinflight=%d/%d\tsync-p50=%v\tsync-p95=%v",
			humanize.Comma(atomic.LoadInt64(&s.fileLines)),
			humanize.Comma(atomic.LoadInt64(&s.decodedKeys)),
			humanize.Comma(atomic.LoadInt64(&s.syncedKeys)),
			atomic.LoadInt64(&s.inflight), s.SyncPara,
			time.Duration(p50),
			time.Duration(p95),
		)
	}
}

// reads all the \n separated lines from a file, write them (without \n) to
// the channel. reads until EOF or stops on the first error encountered
func (s *SyncTask) readLines(input io.Reader, decoders chan<- []byte) error {

	rd := bufio.NewReader(input)

	for {
		line, err := rd.ReadBytes('\n')
		switch err {
		case io.EOF:
			return nil
		case nil:
		default:
			return err
		}

		decoders <- line
		atomic.AddInt64(&s.fileLines, 1)
	}
}

// decodes s3.Keys from a channel of bytes, each byte containing a full key
func (s *SyncTask) decode(wg *sync.WaitGroup, lines <-chan []byte, keys chan<- s3.Key) {
	defer wg.Done()
	var key s3.Key
	for line := range lines {
		err := json.Unmarshal(line, &key)
		if err != nil {
			s.elog.Printf("unmarshaling line: %v", err)
		} else {
			keys <- key
			atomic.AddInt64(&s.decodedKeys, 1)
		}
	}
}

// encode write the keys it receives in JSON to a dst writer.
func (s *SyncTask) encode(wg *sync.WaitGroup, dst io.Writer, keys <-chan s3.Key) {
	defer wg.Done()
	enc := json.NewEncoder(dst)
	for key := range keys {
		err := enc.Encode(key)
		if err != nil {
			s.elog.Fatalf("encoding %q to JSON: %v", key.Key, err)
		}
	}
}

// syncKey uses s.syncMethod to copy keys from `src` to `dst`, until `keys` is
// closed. Each key error is retried MaxRetry times, unless the error is not
// retriable.
func (s *SyncTask) syncKey(wg *sync.WaitGroup, src, dst *s3.Bucket, keys <-chan s3.Key, synced, failed chan<- s3.Key) {
	defer wg.Done()

	for key := range keys {
		retries, err := s.syncOrRetry(src, dst, key)
		// If we exhausted MaxRetry, log the error to the error log
		if err != nil {
			failed <- key

			s.elog.Printf("failed %d times to sync %q", retries, key.Key)
			switch e := err.(type) {
			case *s3.Error: // cannot be abort worthy at this point
				s.elog.Printf("s3-error-code=%q\ts3-error-msg=%q\tkey=%q", e.Code, e.Message, key.Key)
			default:
				s.elog.Printf("other-error=%#v\tkey=%q", e, key.Key)
			}

		} else {
			synced <- key
			atomic.AddInt64(&s.syncedKeys, 1)
		}
	}
}

// syncOrRetry will try to sync a key many times, until it succeeds or
// fail more than MaxRetry times. It will sleep between retries and abort
// the program on errors that are unrecoverable (like bad auths).
func (s *SyncTask) syncOrRetry(src, dst *s3.Bucket, key s3.Key) (int, error) {
	var err error
	retry := 1
	for ; retry <= s.MaxRetry; retry++ {
		start := time.Now()

		// do a put copy call (sync directly from bucket to another
		// without fetching the content locally)
		atomic.AddInt64(&s.inflight, 1)
		err = s.Sync(src, dst, key)
		atomic.AddInt64(&s.inflight, -1)
		s.qtStreamL.Lock()
		s.qtStream.Insert(float64(time.Since(start).Nanoseconds()))
		s.qtStreamL.Unlock()

		switch e := err.(type) {
		case nil:
			// when there are no errors, there's nothing to retry
			return retry, nil
		case *s3.Error:
			// if the error is specific to S3, we can do smart stuff like
			if shouldAbort(e) {
				// abort if its an error that will occur for all future calls
				// such as bad auth, or the bucket not existing anymore (that'd be bad!)
				s.elog.Fatalf("abort-worthy-error=%q\terror-msg=%q\tkey=%#v", e.Code, e.Message, key)
			}
			if !shouldRetry(e) {
				// give up on that key if it's not retriable, such as a key
				// that was deleted
				s.elog.Printf("unretriable-error=%q\terror-msg=%q\tkey=%q", e.Code, e.Message, key.Key)
				return retry, e
			}
			// carry on to retry
		default:
			// carry on to retry
		}

		// log that we sleep, but don't log the error itself just
		// yet (to avoid logging transient network errors that are
		// recovered by retrying)
		sleepFor := s.RetryBase * time.Duration(retry)
		s.elog.Printf("worker-sleep-on-retryiable-error=%v", sleepFor)
		time.Sleep(sleepFor)
		s.elog.Printf("worker-wake-up, retries=%d/%d", retry, s.MaxRetry)

	}
	return retry, err
}

// Classify S3 errors that should be retried.
func shouldRetry(err error) bool {
	switch {
	default:
		// don't retry errors
		return false

		// unless they're one of:
	case s3.IsS3Error(err, s3.ErrExpiredToken):
	case s3.IsS3Error(err, s3.ErrIncompleteBody):
	case s3.IsS3Error(err, s3.ErrInternalError):
	case s3.IsS3Error(err, s3.ErrInvalidBucketState):
	case s3.IsS3Error(err, s3.ErrInvalidObjectState):
	case s3.IsS3Error(err, s3.ErrInvalidPart):
	case s3.IsS3Error(err, s3.ErrInvalidPartOrder):
	case s3.IsS3Error(err, s3.ErrOperationAborted):
	case s3.IsS3Error(err, s3.ErrPermanentRedirect):
	case s3.IsS3Error(err, s3.ErrPreconditionFailed):
	case s3.IsS3Error(err, s3.ErrRedirect):
	case s3.IsS3Error(err, s3.ErrRequestTimeout):
	case s3.IsS3Error(err, s3.ErrRequestTimeTooSkewed):
	case s3.IsS3Error(err, s3.ErrServiceUnavailable):
	case s3.IsS3Error(err, s3.ErrTemporaryRedirect):
	case s3.IsS3Error(err, s3.ErrTokenRefreshRequired):
	case s3.IsS3Error(err, s3.ErrUnexpectedContent):
	case s3.IsS3Error(err, s3.ErrSlowDown):
	}
	return true
}

// Classify S3 errors that require aborting the whole sync process.
func shouldAbort(err error) bool {
	switch {
	default:
		// don't abort on errors
		return false

		// unless they're one of:
	case s3.IsS3Error(err, s3.ErrAccessDenied):
	case s3.IsS3Error(err, s3.ErrAccountProblem):
	case s3.IsS3Error(err, s3.ErrCredentialsNotSupported):
	case s3.IsS3Error(err, s3.ErrInvalidAccessKeyID):
	case s3.IsS3Error(err, s3.ErrInvalidBucketName):
	case s3.IsS3Error(err, s3.ErrNoSuchBucket):
	}
	return true
}
