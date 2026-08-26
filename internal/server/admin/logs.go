package admin

import (
	"context"
	"log/slog"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// GetLogsRequest is a cursor-style query over the in-process slog ring buffer.
// Pass Since=0 on the first call
// to receive the whole ring; on subsequent calls pass back LastSeq from
// the previous response to receive only newer records.
type GetLogsRequest struct {
	Since uint64 `json:"since"`
	// Limit caps the number of records returned, applied after the
	// since-cursor filter. Zero means "no extra cap" — the response is
	// still bounded by the ring's capacity.
	Limit int `json:"limit"`
}

// LogEntry is the wire shape of one record. Attrs are flattened to a
// map for the SPA's convenience; key collisions in the original record
// (rare; slog allows duplicates in principle) keep the last value.
type LogEntry struct {
	Seq   uint64            `json:"seq"`
	Time  string            `json:"time"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs"`
}

// GetLogs returns log records produced by the atlantis server since the
// caller's cursor. Reads from a fixed-size in-process ring (see
// internal/obs/logring.go); the writer side is lock-free so this read
// adds no contention to log emit on hot RPC paths.
//
// Returns an empty response when LogRing was not configured at boot —
// for tests and the legacy single-binary path where the ring isn't
// installed.
func (s *Service) GetLogs(ctx context.Context, req *adminpb.GetLogsRequest) (*adminpb.GetLogsResponse, error) {
	if s.logRing == nil {
		return &adminpb.GetLogsResponse{}, nil
	}

	recs, head := s.logRing.Since(req.GetSince())
	if limit := int(req.GetLimit()); limit > 0 && len(recs) > limit {
		// Keep the newest L entries — the SPA renders tail-anchored.
		recs = recs[len(recs)-limit:]
	}

	out := make([]*adminpb.LogEntry, len(recs))
	for i, r := range recs {
		attrs := make(map[string]string, len(r.Attrs))
		for _, kv := range r.Attrs {
			attrs[kv.Key] = kv.Val
		}
		out[i] = &adminpb.LogEntry{
			Seq:   r.Seq,
			Time:  r.Time.UTC().Format(time.RFC3339Nano),
			Level: slogLevelName(r.Level),
			Msg:   r.Msg,
			Attrs: attrs,
		}
	}

	return &adminpb.GetLogsResponse{Records: out, LastSeq: head}, nil
}

func slogLevelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
