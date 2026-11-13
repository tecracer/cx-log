package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"time"

	"github.com/megaproaktiv/agentic-cx-designer-sdk-go/acxd"
)

// logQuerier is the subset of *acxd.Client the tailer needs.
type logQuerier interface {
	QueryLogs(ctx context.Context, params *acxd.QueryLogsRequest, optFns ...func(*acxd.Options)) (*acxd.QueryLogsResponse, error)
}

type tailOptions struct {
	Lines      int
	Follow     bool
	Since      time.Duration
	Interval   time.Duration
	Lag        time.Duration
	PageSize   int
	Filter     *acxd.LogSearchFilter
	EventTypes map[acxd.LogEventType]bool
	Printer    printer
	Debug      bool
}

type tailer struct {
	client logQuerier
	opts   tailOptions
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error

	// seen holds fingerprints of printed events with their event time, so
	// overlapping follow windows never print an event twice.
	seen map[uint64]time.Time
}

func newTailer(c logQuerier, o tailOptions) *tailer {
	return &tailer{client: c, opts: o, now: time.Now, sleep: sleepCtx, seen: map[uint64]time.Time{}}
}

func (t *tailer) Run(ctx context.Context) error {
	end := t.now().UTC()

	if t.opts.Lines > 0 {
		if err := t.printLast(ctx, end.Add(-t.opts.Since), end); err != nil {
			return err
		}
	}
	if !t.opts.Follow {
		return nil
	}

	for {
		if err := t.sleep(ctx, t.opts.Interval); err != nil {
			return err
		}
		start := end.Add(-t.opts.Lag)
		end = t.now().UTC()
		if err := t.printRange(ctx, start, end); err != nil {
			if !retryable(err) {
				return err
			}
			fmt.Fprintf(os.Stderr, "cx-log: %v (retrying)\n", err)
			end = start.Add(t.opts.Lag) // re-scan the same window next time
			continue
		}
		t.prune(start)
	}
}

// printLast prints the newest opts.Lines matching events in [start, end],
// oldest first, like tail -n.
func (t *tailer) printLast(ctx context.Context, start, end time.Time) error {
	var newest []acxd.LogEntry // newest first
	err := t.query(ctx, start, end, acxd.SortOrderDesc, func(e acxd.LogEntry) bool {
		if t.match(e) {
			newest = append(newest, e)
		}
		return len(newest) < t.opts.Lines
	})
	if err != nil {
		return err
	}
	for i := len(newest) - 1; i >= 0; i-- {
		t.emit(newest[i])
	}
	return nil
}

// printRange prints every not-yet-seen matching event in [start, end], oldest first.
func (t *tailer) printRange(ctx context.Context, start, end time.Time) error {
	return t.query(ctx, start, end, acxd.SortOrderAsc, func(e acxd.LogEntry) bool {
		if t.match(e) {
			t.emit(e)
		}
		return true
	})
}

// query pages through QueryLogs, calling fn per entry until fn returns false
// or there are no more pages.
func (t *tailer) query(ctx context.Context, start, end time.Time, order acxd.SortOrder, fn func(acxd.LogEntry) bool) error {
	size := int32(t.opts.PageSize)
	req := &acxd.QueryLogsRequest{
		TimeFilter:   acxd.AbsoluteTimeFilter{StartTimestamp: start, EndTimestamp: end},
		SearchFilter: t.opts.Filter,
		SortOrder:    order,
		MaxResults:   &size,
	}
	for {
		out, err := t.queryWithRetry(ctx, req)
		if err != nil {
			return err
		}
		if t.opts.Debug {
			fmt.Fprintf(os.Stderr, "cx-log: debug: window=%s..%s order=%s items=%d progress=%.1f%% scanned=%dB nextToken=%t\n",
				start.Format(time.RFC3339), end.Format(time.RFC3339), order, len(out.Items),
				out.QueryStatus.ProgressPercentage, out.QueryStatus.CumulativeBytesScanned, out.NextToken != nil)
		}
		for _, e := range out.Items {
			if !fn(e) {
				return nil
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return nil
		}
		req.NextToken = out.NextToken
	}
}

// queryWithRetry retries throttling and server errors a few times,
// honoring RetryAfterSeconds when the service sends it.
func (t *tailer) queryWithRetry(ctx context.Context, req *acxd.QueryLogsRequest) (*acxd.QueryLogsResponse, error) {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		out, err := t.client.QueryLogs(ctx, req)
		if err == nil || attempt == 5 || !retryable(err) {
			if err != nil {
				return nil, fmt.Errorf("query logs: %w", err)
			}
			return out, nil
		}
		wait := backoff
		if s := retryAfter(err); s > 0 {
			wait = s
		}
		if err := t.sleep(ctx, wait); err != nil {
			return nil, err
		}
		backoff *= 2
	}
}

func retryable(err error) bool {
	var th *acxd.ThrottlingException
	var is *acxd.InternalServerException
	return errors.As(err, &th) || errors.As(err, &is)
}

func retryAfter(err error) time.Duration {
	var th *acxd.ThrottlingException
	var is *acxd.InternalServerException
	switch {
	case errors.As(err, &th) && th.RetryAfterSeconds != nil:
		return time.Duration(*th.RetryAfterSeconds) * time.Second
	case errors.As(err, &is) && is.RetryAfterSeconds != nil:
		return time.Duration(*is.RetryAfterSeconds) * time.Second
	}
	return 0
}

func (t *tailer) match(e acxd.LogEntry) bool {
	return len(t.opts.EventTypes) == 0 || t.opts.EventTypes[e.EventType]
}

func (t *tailer) emit(e acxd.LogEntry) {
	id := fingerprint(e)
	if _, dup := t.seen[id]; dup {
		return
	}
	t.seen[id] = e.EventTime
	t.opts.Printer.Print(e)
}

// prune forgets events older than the next window's start; they can no
// longer be returned again, so the seen set stays bounded.
func (t *tailer) prune(before time.Time) {
	for id, ts := range t.seen {
		if ts.Before(before) {
			delete(t.seen, id)
		}
	}
}

// fingerprint identifies an event; LogEntry has no ID field, so hash its content.
func fingerprint(e acxd.LogEntry) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d\x00%s", e.EventTime.UnixNano(), e.EventType)
	for _, props := range [][]acxd.LogProperty{e.CommonProperties, e.EventProperties} {
		h.Write([]byte{1})
		for _, p := range props {
			fmt.Fprintf(h, "%s\x00%s\x00", p.Key, p.Value)
		}
	}
	return h.Sum64()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}
