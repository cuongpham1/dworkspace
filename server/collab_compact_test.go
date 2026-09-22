package server

import (
	"encoding/json"
	"testing"
)

// When the CRDT update log gets folded back into a snapshot.
//
// The server cannot do the folding itself — a snapshot is the CRDT's merged
// state and there is no Yjs implementation on this side — so it asks a
// connected client for one. That makes WHEN it asks the only lever there is,
// and the original answer, "after 200 rows", measured the wrong quantity. Two
// hundred updates is a few kilobytes of ordinary typing and megabytes once
// somebody pastes a table into a page and keeps editing; rooms in the second
// shape sat well under the row count while their log grew past anything the
// snapshot would have cost. yjs_updates had reached 40 MB that way.
//
// It also never asked at the one moment it most usefully could: a log left
// behind by an editor who closed their tab has nobody to ask, and stays exactly
// as it is until somebody opens the page again — which is precisely when a
// client holding the full replayed state is available.

// pendingRoom builds a room whose log is already at the given size, with one
// connection able to receive a request.
func pendingRoom(rows, bytes int64) (*collabRoom, *collabConn) {
	conn := &collabConn{out: make(chan outMsg, 4), done: make(chan struct{}), aware: map[uint64]uint64{}}
	return &collabRoom{
		pageID:       "p1",
		conns:        map[*collabConn]struct{}{conn: {}},
		loaded:       true,
		seq:          rows,
		pending:      rows,
		pendingBytes: bytes,
	}, conn
}

// asked reports the seq a snapshot was requested up to, or 0 for no request.
func asked(t *testing.T, conn *collabConn) int64 {
	t.Helper()
	select {
	case m := <-conn.out:
		var req struct {
			SnapshotRequest int64 `json:"snapshotRequest"`
		}
		if err := json.Unmarshal(m.data, &req); err != nil {
			t.Fatalf("not a snapshot request: %q", m.data)
		}
		return req.SnapshotRequest
	default:
		return 0
	}
}

func TestCompactionTriggersOnSizeNotOnlyOnCount(t *testing.T) {
	// Twelve updates — nowhere near the row limit — but a third of a megabyte.
	// This is the paste-a-table case, and it used to grow unbounded.
	room, conn := pendingRoom(12, compactBytes+1)
	room.requestSnapshotLocked(conn, room.seq)
	if got := asked(t, conn); got != 12 {
		t.Errorf("no snapshot requested for a %d-byte log (got %d)", compactBytes+1, got)
	}

	// And a log that is small both ways is left alone: asking costs the client
	// a full-document serialisation, so it must not happen on every keystroke.
	room, conn = pendingRoom(12, 4096)
	room.requestSnapshotLocked(conn, room.seq)
	if got := asked(t, conn); got != 0 {
		t.Errorf("a 4 KB log was compacted anyway (up to seq %d)", got)
	}
}

func TestCompactionStillTriggersOnCount(t *testing.T) {
	// Many tiny updates: the row limit is what catches this one, and it has to
	// keep working now that it is no longer the only limit.
	room, conn := pendingRoom(compactThreshold, 900)
	room.requestSnapshotLocked(conn, room.seq)
	if got := asked(t, conn); got != compactThreshold {
		t.Errorf("no snapshot requested after %d updates (got %d)", compactThreshold, got)
	}
}

func TestOnlyOneClientIsAskedAtATime(t *testing.T) {
	room, conn := pendingRoom(12, compactBytes+1)
	room.requestSnapshotLocked(conn, room.seq)
	if asked(t, conn) == 0 {
		t.Fatal("the first request never went out")
	}
	// A second ask while the first is in flight would have two clients each
	// sending a full document, and applySnapshot accepts only one of them.
	room.requestSnapshotLocked(conn, room.seq)
	if got := asked(t, conn); got != 0 {
		t.Errorf("asked again while a snapshot was already in flight (seq %d)", got)
	}
}

// The abandoned-log case: the room is rebuilt from the database on join, with
// pendingBytes carrying what was found there, and the joiner is asked.
func TestAJoinerIsAskedToFoldABacklogLeftBehind(t *testing.T) {
	room, conn := pendingRoom(40, compactBytes*3)
	// Exactly what handleCollab does after sending the replay and {"synced":true}.
	room.requestSnapshotLocked(conn, room.seq)
	if got := asked(t, conn); got != 40 {
		t.Errorf("a %d KB log sat untouched through a fresh join (got seq %d)",
			compactBytes*3/1024, got)
	}
}
