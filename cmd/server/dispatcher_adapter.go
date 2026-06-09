// Thin adapter that lets the admin Service treat a
// *jobsdispatcher.Dispatcher as an admin.WorkerDispatcher without
// admin importing jobsdispatcher.
//
// The admin package owns a `WorkerDispatcher` interface scoped to
// the four console-facing operations (Snapshot, Get, Drain, Evict).
// jobsdispatcher.Dispatcher already exposes those methods with its
// own DTO types; we adapt by translating between the two struct
// shapes. The structs are intentionally identical field-for-field
// so the translation is mechanical.

package main

import (
	"github.com/rachitkumar205/atlantis/internal/server/admin"
	"github.com/rachitkumar205/atlantis/internal/server/jobsdispatcher"
)

type dispatcherAdapter struct {
	d *jobsdispatcher.Dispatcher
}

func newDispatcherAdapter(d *jobsdispatcher.Dispatcher) admin.WorkerDispatcher {
	if d == nil {
		return nil
	}
	return &dispatcherAdapter{d: d}
}

func (a *dispatcherAdapter) SnapshotSessions() []admin.DispatcherSessionSnapshot {
	in := a.d.SnapshotSessions()
	out := make([]admin.DispatcherSessionSnapshot, len(in))
	for i, s := range in {
		out[i] = adaptSnapshot(s)
	}
	return out
}

func (a *dispatcherAdapter) GetSession(sessionID string) (admin.DispatcherSessionDetail, bool) {
	d, ok := a.d.GetSession(sessionID)
	if !ok {
		return admin.DispatcherSessionDetail{}, false
	}
	out := admin.DispatcherSessionDetail{
		DispatcherSessionSnapshot: adaptSnapshot(d.SessionSnapshot),
		JobNames:                  d.JobNames,
	}
	for _, r := range d.Inflight {
		out.Inflight = append(out.Inflight, admin.DispatcherInflightDetail{
			JobID:        r.JobID,
			JobName:      r.JobName,
			DispatchedAt: r.DispatchedAt,
			AckReceived:  r.AckReceived,
		})
	}
	for _, e := range d.Events {
		out.Events = append(out.Events, admin.DispatcherEventSnapshot{
			At:      e.At,
			Kind:    e.Kind,
			JobID:   e.JobID,
			JobName: e.JobName,
			Note:    e.Note,
		})
	}
	return out, true
}

func (a *dispatcherAdapter) DrainSession(sessionID string) error {
	err := a.d.DrainSession(sessionID)
	if err == jobsdispatcher.ErrSessionNotFound {
		return admin.ErrWorkerSessionNotFound
	}
	return err
}

func (a *dispatcherAdapter) EvictSession(sessionID string) error {
	err := a.d.EvictSession(sessionID)
	if err == jobsdispatcher.ErrSessionNotFound {
		return admin.ErrWorkerSessionNotFound
	}
	return err
}

func adaptSnapshot(s jobsdispatcher.SessionSnapshot) admin.DispatcherSessionSnapshot {
	return admin.DispatcherSessionSnapshot{
		SessionID:       s.SessionID,
		Caller:          s.Caller,
		Queue:           s.Queue,
		PodID:           s.PodID,
		SDKVersion:      s.SDKVersion,
		ConnectedAt:     s.ConnectedAt,
		LastHeartbeatAt: s.LastHeartbeatAt,
		MaxInFlight:     s.MaxInFlight,
		InflightCount:   s.InflightCount,
		Dispatched:      s.Dispatched,
		Completed:       s.Completed,
		Failed:          s.Failed,
		Revoked:         s.Revoked,
		Drained:         s.Drained,
	}
}
