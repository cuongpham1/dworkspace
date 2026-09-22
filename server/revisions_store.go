package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"regexp"
	"sort"
	"strings"
)

// How a page revision is stored (see history.go for what a revision IS).
//
// A revision is a verbatim snapshot of BlockNote JSON, kept 50 deep per page.
// That is deliberately redundant — the point of history is to still have the
// text after somebody deleted it — but the redundancy was being paid at full
// price: on the instance this was measured against, 3,795 revisions came to
// 106 MB, a fifth of the whole database, for pages whose live content was 43 MB
// in total. BlockNote JSON is extremely repetitive (every paragraph carries the
// same dozen style keys), and gzip gets roughly 8:1 on it.
//
// So the content column is written compressed, into content_gz. The plain
// column stays for rows that do not shrink (a very short revision, where the
// gzip header costs more than it saves) and for every row written before this
// existed — reads accept either, and nothing has to be migrated before the
// feature works.
//
// The one thing compression breaks is SEARCHING the column, and exactly one
// caller did that: removeUnreferencedFiles (lifecycle_account.go) asks "does
// any revision still mention this upload?" before deleting it from disk. A
// gzip blob matches no LIKE pattern, so that question would have started
// answering "no" for every compressed revision and the cleanup would have
// deleted images out from under restorable history. The assets column exists
// for that one question: the file references, extracted at write time and kept
// in the clear, where they cost a few dozen bytes per revision.

// uploadRefRe matches the upload references that removeUnreferencedFiles looks for.
// Deliberately generous on the name: it only has to be a superset of what the
// LIKE pattern would have found, because a false positive merely keeps a file
// a little longer, while a miss deletes one that is still needed.
var uploadRefRe = regexp.MustCompile(`/files/[A-Za-z0-9._\-]+`)

// revisionAssets pulls the upload references out of a revision's content, sorted
// and de-duplicated. The leading and trailing separator let a LIKE pattern for
// "/files/x" not also match "/files/xy".
func revisionAssets(content string) string {
	found := uploadRefRe.FindAllString(content, -1)
	if len(found) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var uniq []string
	for _, f := range found {
		if !seen[f] {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	sort.Strings(uniq)
	return "\n" + strings.Join(uniq, "\n") + "\n"
}

// encodeRevision prepares one revision for storage: the three column values,
// in the order (content, content_gz, assets).
//
// Compression is skipped unless it actually pays — below that threshold the
// row is stored as it always was, and the read path cannot tell the difference.
func encodeRevision(content string) (string, []byte, string) {
	assets := revisionAssets(content)
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return content, nil, assets
	}
	if _, err := io.WriteString(zw, content); err != nil {
		return content, nil, assets
	}
	if err := zw.Close(); err != nil {
		return content, nil, assets
	}
	// A tenth is the margin at which the extra column and the decompression on
	// every read start being worth it; short revisions fall below it and stay
	// plain.
	if buf.Len() >= len(content)-len(content)/10 {
		return content, nil, assets
	}
	return "", buf.Bytes(), assets
}

// decodeRevision reads a revision back from whichever of the two columns holds
// it. A revision that cannot be decompressed is returned as the empty document
// rather than as garbage: the history list keeps working, and the one broken
// entry opens empty instead of feeding a corrupt blob into the editor.
func decodeRevision(content string, gz []byte) string {
	if len(gz) == 0 {
		return content
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		log.Printf("revision: unreadable compressed content: %v", err)
		return "[]"
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		log.Printf("revision: truncated compressed content: %v", err)
		return "[]"
	}
	return string(out)
}

// revisionsCompressed is the version of this storage layout. Raising it makes
// the next start walk the table again.
const revisionsCompressed = "1"

// compressRevisions rewrites revisions that predate the compressed column.
//
// Walks the table by id rather than by a "not done yet" predicate, because a
// revision that does not compress is left exactly as it was and would match any
// such predicate forever. Batched: this holds the single write connection while
// a server is starting up, and the whole table is far more than fits in memory.
// Re-running is harmless — an already-compressed row is recognised and skipped.
func (s *Server) compressRevisions() error {
	if s.setting("revisions_compressed", "0") == revisionsCompressed {
		return nil
	}
	type rev struct {
		id, content string
		gz          []byte
	}
	cursor := ""
	total, saved := 0, 0
	for {
		rows, err := s.db.Query(`SELECT id, content, content_gz FROM page_revisions
			WHERE id > ? ORDER BY id LIMIT 200`, cursor)
		if err != nil {
			return err
		}
		var batch []rev
		for rows.Next() {
			var r rev
			if rows.Scan(&r.id, &r.content, &r.gz) == nil {
				batch = append(batch, r)
			}
		}
		rows.Close() // drain before writing — one connection
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			cursor = r.id
			if len(r.gz) > 0 || r.content == "" {
				continue // already stored compressed
			}
			content, gz, assets := encodeRevision(r.content)
			if len(gz) == 0 {
				continue // too short to be worth it — leave the row alone
			}
			if _, err := s.db.Exec(`UPDATE page_revisions SET content = ?, content_gz = ?, assets = ? WHERE id = ?`,
				content, gz, assets, r.id); err != nil {
				return err
			}
			total++
			saved += len(r.content) - len(gz)
		}
	}
	s.setSetting("revisions_compressed", revisionsCompressed)
	if total > 0 {
		log.Printf("page history: compressed %d revisions, %.1f MB saved", total, float64(saved)/(1<<20))
	}
	return nil
}
