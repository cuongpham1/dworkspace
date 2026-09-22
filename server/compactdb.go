package server

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// `dworkspace compact` — what the database is spending its size on, and the two
// things that get it back.
//
// The question this answers came from an admin looking at a 564 MiB file for 22
// people and concluding the server would fill up. It would not: of that file,
// 43 MB was the text those 22 people had written. The rest was mechanics —
// deleted pages still inside the retention window, revision history stored
// verbatim, and a search index that kept its own copy of every word. Two of
// those three now cost a fraction of what they did (revisions_store.go,
// searchTablesDDL); this command is for the third, which is not a storage
// question but a decision only a person can make: the trash is somebody's
// undo, and emptying it early throws that away.
//
// VACUUM is here because SQLite never shrinks a file on its own. Deleting rows
// frees pages for REUSE, which is why the file does not grow either — but after
// a one-off clear-out of a third of the database, the difference between "will
// be reused eventually" and "is free on the disk now" is the whole point.
//
// Run it with the server stopped. It takes the sole SQLite connection, and
// VACUUM rewrites the entire file.

// DBReport is the size breakdown, in bytes of stored text unless stated.
type DBReport struct {
	FileSize     int64
	LivePages    int
	LiveBytes    int64
	TrashedPages int
	TrashedBytes int64
	Revisions    int
	RevisionMB   int64
	ChunkBytes   int64
	YjsBytes     int64
}

func readDBReport(db *sql.DB, path string) (DBReport, error) {
	var r DBReport
	if fi, err := os.Stat(path); err == nil {
		r.FileSize = fi.Size()
	}
	q := func(sqlText string, dest ...any) error {
		return db.QueryRow(sqlText).Scan(dest...)
	}
	if err := q(`SELECT COUNT(*), COALESCE(SUM(LENGTH(content)), 0) FROM pages WHERE trashed_at IS NULL`,
		&r.LivePages, &r.LiveBytes); err != nil {
		return r, err
	}
	if err := q(`SELECT COUNT(*), COALESCE(SUM(LENGTH(content)), 0) FROM pages WHERE trashed_at IS NOT NULL`,
		&r.TrashedPages, &r.TrashedBytes); err != nil {
		return r, err
	}
	// Whichever column the revision is in — see revisions_store.go.
	if err := q(`SELECT COUNT(*), COALESCE(SUM(LENGTH(content) + LENGTH(COALESCE(content_gz, ''))), 0) FROM page_revisions`,
		&r.Revisions, &r.RevisionMB); err != nil {
		return r, err
	}
	q(`SELECT COALESCE(SUM(LENGTH(text)), 0) FROM page_chunks`, &r.ChunkBytes)
	q(`SELECT COALESCE(SUM(LENGTH(data)), 0) FROM yjs_updates`, &r.YjsBytes)
	return r, nil
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

// Print writes the breakdown in the order that answers "where did it go".
func (r DBReport) Print(label string) {
	fmt.Printf("\n%s — %s on disk\n", label, mb(r.FileSize))
	fmt.Printf("  live pages        %6d   %s   ← what people wrote\n", r.LivePages, mb(r.LiveBytes))
	fmt.Printf("  in the trash      %6d   %s\n", r.TrashedPages, mb(r.TrashedBytes))
	fmt.Printf("  revisions         %6d   %s\n", r.Revisions, mb(r.RevisionMB))
	fmt.Printf("  search passages            %s\n", mb(r.ChunkBytes))
	fmt.Printf("  pending CRDT log           %s\n", mb(r.YjsBytes))
}

// CompactDB reports the breakdown and, when emptyTrash is set, permanently
// deletes everything in the trash before rewriting the file.
//
// emptyTrash is not a cleanup flag with a sensible default — it destroys pages
// their owners can currently still restore. The caller asks for it explicitly
// or it does not happen.
func CompactDB(dataDir string, emptyTrash bool) error {
	path := filepath.Join(dataDir, DBFile)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no database at %s", path)
	}
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	s := &Server{db: db}
	// openDB migrates the schema but not the search index — that rebuild belongs
	// to a server start (searchindex.go). Emptying the trash reaches into
	// chunks_fts, so refuse rather than fail halfway through with an SQL error
	// about a column nobody asked about.
	if emptyTrash {
		if _, err := db.Exec(`SELECT seq FROM page_chunks LIMIT 1`); err != nil {
			return fmt.Errorf("the search index is from an older version — start the server once to rebuild it, then run this again")
		}
	}

	before, err := readDBReport(db, path)
	if err != nil {
		return err
	}
	before.Print("Before")

	if emptyTrash {
		// Everything, not "older than the retention window": the point of asking
		// for this is that waiting out the window is what you did not want.
		n, err := s.PurgeTrashedBefore(now())
		if err != nil {
			return fmt.Errorf("emptying the trash: %w", err)
		}
		fmt.Printf("\nPermanently deleted %d trashed pages.\n", n)
	}

	// WAL contents have to land in the main file before VACUUM, or the pages
	// freed a moment ago are still sitting in the journal.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	fmt.Print("\nRewriting the database… ")
	start := time.Now()
	if _, err := db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	fmt.Printf("done in %s\n", time.Since(start).Round(time.Millisecond))

	after, err := readDBReport(db, path)
	if err != nil {
		return err
	}
	after.Print("After")
	fmt.Printf("\nReclaimed %s.\n\n", mb(before.FileSize-after.FileSize))
	return nil
}
