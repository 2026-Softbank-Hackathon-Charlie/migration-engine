package engine

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

type snapshotDB struct {
	Database
	recovery, pending          bool
	started                    time.Time
	err                        error
	reads, closes, legacyReads int
	read                       func(int) (bool, bool, time.Time, error)
}

func (d *snapshotDB) SettleStatus(context.Context) (bool, bool, time.Time, error) {
	d.reads++
	if d.read != nil {
		return d.read(d.reads)
	}
	return d.recovery, d.pending, d.started, d.err
}
func (d *snapshotDB) InRecovery(context.Context) (bool, error) {
	d.legacyReads++
	return false, errors.New("unexpected legacy read")
}
func (d *snapshotDB) PendingRestart(context.Context) (bool, error) {
	d.legacyReads++
	return false, errors.New("unexpected legacy read")
}
func (d *snapshotDB) PostmasterStart(context.Context) (time.Time, error) {
	d.legacyReads++
	return time.Time{}, errors.New("unexpected legacy read")
}
func (d *snapshotDB) Close() { d.closes++ }

func TestObserveSettledCombinedSignalsAndFailure(t *testing.T) {
	failed := errors.New("observation failed")
	for _, tc := range []struct {
		name              string
		recovery, pending bool
		err               error
	}{
		{"writable", false, false, nil}, {"recovery", true, false, nil}, {"pending restart", false, true, nil}, {"failure", false, false, failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			e := h.engine(t, "")
			started := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
			d := &snapshotDB{Database: h.w.dstDB, recovery: tc.recovery, pending: tc.pending, started: started, err: tc.err}
			e.connect = func(context.Context, string) (Database, error) { return d, nil }
			s, err := e.observeSettled(t.Context(), e.dst, h.p.Target.DSN)
			if !errors.Is(err, tc.err) || s.inRecovery != tc.recovery || s.pending != tc.pending || !s.server.Equal(started) {
				t.Fatalf("sample=%+v err=%v", s, err)
			}
			if d.reads != 1 || d.closes != 1 || d.legacyReads != 0 {
				t.Fatalf("reads=%d closes=%d legacy=%d", d.reads, d.closes, d.legacyReads)
			}
		})
	}
}

func TestObserveSettledLegacyAdapterStillSupported(t *testing.T) {
	h := newHarness(t)
	h.w.dstDB.pendingRestart = true
	e := h.engine(t, "")
	s, err := e.observeSettled(t.Context(), e.dst, h.p.Target.DSN)
	if err != nil || !s.inRecovery || !s.pending || s.server.IsZero() || h.w.dstDB.pmCalls != 1 {
		t.Fatalf("sample=%+v err=%v reads=%d", s, err, h.w.dstDB.pmCalls)
	}
}

func TestCombinedSettlePreservesRestartDetectionAndFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		e := h.engine(t, "")
		started := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
		d := &snapshotDB{Database: h.w.dstDB, read: func(n int) (bool, bool, time.Time, error) {
			if n == 1 {
				return false, true, started, nil
			}
			if n == 2 {
				return false, false, started, errors.New("restart disconnect")
			}
			return false, false, started.Add(time.Second), nil
		}}
		e.connect = func(context.Context, string) (Database, error) { return d, nil }
		var slot Database
		begin := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		rep, err := e.waitSettled(ctx, e.dst, h.p.Target.DSN, &slot)
		if err != nil || rep.Restarts != 1 || rep.Unobserved != 1 || rep.PendingRestart != 1 {
			t.Fatalf("report=%+v err=%v", rep, err)
		}
		if time.Since(begin) < settleMin || d.reads < 4 || d.closes != d.reads || d.legacyReads != 0 {
			t.Fatalf("elapsed=%v reads=%d closes=%d legacy=%d", time.Since(begin), d.reads, d.closes, d.legacyReads)
		}
	})
}

func TestCombinedSettleReadErrorsRemainUnobserved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		e := h.engine(t, "")
		d := &snapshotDB{Database: h.w.dstDB, err: errors.New("observation failed")}
		e.connect = func(context.Context, string) (Database, error) { return d, nil }
		var slot Database
		ctx, cancel := context.WithTimeout(t.Context(), 5*settleMin)
		defer cancel()
		rep, err := e.waitSettled(ctx, e.dst, h.p.Target.DSN, &slot)
		if !errors.Is(err, context.DeadlineExceeded) || rep.Unobserved != rep.Polls || d.closes != d.reads || d.legacyReads != 0 {
			t.Fatalf("report=%+v err=%v reads=%d closes=%d legacy=%d", rep, err, d.reads, d.closes, d.legacyReads)
		}
	})
}

func TestSettleDurationsAccumulateBeforeRounding(t *testing.T) {
	var d settleDurations
	for range 5 {
		d.add(settleDurations{clusterRead: 300 * time.Microsecond, connect: 300 * time.Microsecond, databaseRead: 300 * time.Microsecond, close: 300 * time.Microsecond})
	}
	var report SettleReport
	d.report(&report)
	if report.ClusterReadMillis != 1 || report.ConnectMillis != 1 || report.DatabaseReadMillis != 1 || report.CloseMillis != 1 || report.ObservationMillis != 6 {
		t.Fatalf("durations were rounded per poll: %+v", report)
	}
}

func TestFailedSettleSampleIncludesReadAndCloseTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		e := h.engine(t, "")
		d := &timedSnapshotDB{snapshotDB: snapshotDB{Database: h.w.dstDB}}
		e.connect = func(context.Context, string) (Database, error) { time.Sleep(3 * time.Millisecond); return d, nil }
		s, err := e.observeSettled(t.Context(), e.dst, h.p.Target.DSN)
		if err == nil || s.durations.connect != 3*time.Millisecond || s.durations.databaseRead != 4*time.Millisecond || s.durations.close != 5*time.Millisecond {
			t.Fatalf("sample=%+v err=%v", s, err)
		}
	})
}

type timedSnapshotDB struct{ snapshotDB }

func (d *timedSnapshotDB) SettleStatus(context.Context) (bool, bool, time.Time, error) {
	time.Sleep(4 * time.Millisecond)
	return false, false, time.Time{}, errors.New("read failed")
}
func (d *timedSnapshotDB) Close() { time.Sleep(5 * time.Millisecond) }
