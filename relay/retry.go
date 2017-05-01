package relay

import (
	"bytes"
	"sync"
	"time"
)

const (
	retryInitial    = 500 * time.Millisecond
	retryMultiplier = 2
)

type Operation func() error

// Buffers and retries operations, if the buffer is full operations are dropped.
// Only tries one operation at a time, the next operation is not attempted
// until success or timeout of the previous operation.
// There is no delay between attempts of different operations.
type retryBuffer struct {
	initialInterval time.Duration
	multiplier      time.Duration
	maxInterval     time.Duration

	maxBuffered int
	maxBatch    int

	list *bufferList

	p writer
}

func newRetryBuffer(size, batch int, max time.Duration, p writer) *retryBuffer {
	r := &retryBuffer{
		initialInterval: retryInitial,
		multiplier:      retryMultiplier,
		maxInterval:     max,
		maxBuffered:     size,
		maxBatch:        batch,
		list:            newBufferList(size, batch),
		p:               p,
	}
	go r.run()
	return r
}

func (r *retryBuffer) write(buf []byte, query string, auth string) (*responseData, error) {
	buf1 := make([]byte, len(buf))
	copy(buf1, buf)
	_, err := r.list.add(buf1, query, auth)
	if err != nil {
		return nil, err
	}

	return &responseData{
		StatusCode: 204,
	}, nil
}

func (r *retryBuffer) run() {
	buf := bytes.NewBuffer(make([]byte, 0, r.maxBatch))
	for {
		buf.Reset()
		batch := r.list.pop()

		for _, b := range batch.bufs {
			buf.Write(b)
		}

		interval := r.initialInterval
		for {
			resp, err := r.p.write(buf.Bytes(), batch.query, batch.auth)
			if err == nil && resp.StatusCode/100 != 5 {
				batch.resp = resp
				break
			}

			if interval != r.maxInterval {
				interval *= r.multiplier
				if interval > r.maxInterval {
					interval = r.maxInterval
				}
			}

			time.Sleep(interval)
		}
	}
}

type batch struct {
	query string
	auth  string
	bufs  [][]byte
	size  int
	full  bool

	resp *responseData

	next *batch
}

func newBatch(buf []byte, query string, auth string) *batch {
	b := new(batch)
	b.bufs = [][]byte{buf}
	b.size = len(buf)
	b.query = query
	b.auth = auth
	return b
}

type bufferList struct {
	cond     *sync.Cond
	head     *batch
	size     int
	maxSize  int
	maxBatch int
}

func newBufferList(maxSize, maxBatch int) *bufferList {
	return &bufferList{
		cond:     sync.NewCond(new(sync.Mutex)),
		maxSize:  maxSize,
		maxBatch: maxBatch,
	}
}

// pop will remove and return the first element of the list, blocking if necessary
func (l *bufferList) pop() *batch {
	l.cond.L.Lock()

	for l.size == 0 {
		l.cond.Wait()
	}

	b := l.head
	l.head = l.head.next
	l.size -= b.size

	l.cond.L.Unlock()

	return b
}

func (l *bufferList) add(buf []byte, query string, auth string) (*batch, error) {
	l.cond.L.Lock()

	if l.size+len(buf) > l.maxSize {
		l.cond.L.Unlock()
		return nil, ErrBufferFull
	}

	l.size += len(buf)
	l.cond.Signal()

	var cur **batch

	// non-nil batches that either don't match the query string, don't match the auth
	// credentials, or would be too large when adding the current set of points
	// (auth must be checked to prevent potential problems in multi-user scenarios)
	for cur = &l.head; *cur != nil; cur = &(*cur).next {
		if (*cur).query != query || (*cur).auth != auth || (*cur).full {
			continue
		}

		if (*cur).size+len(buf) > l.maxBatch {
			// prevent future writes from preceding this write
			(*cur).full = true
			continue
		}

		break
	}

	if *cur == nil {
		// new tail element
		*cur = newBatch(buf, query, auth)
	} else {
		// append to current batch
		b := *cur
		b.size += len(buf)
		b.bufs = append(b.bufs, buf)
	}

	defer l.cond.L.Unlock()
	return *cur, nil
}
