package server

import (
	"fmt"
	"log"
	"strings"
	"unicode"
)

// i18n-ok-file: the German IS the subject here — this file implements folding
// and stemming for German text, and the examples ("Verträge" → "vertrag") are
// what the reasoning rests on. Translating them would delete the explanation.
// The cost is real: new German prose in THIS file is not caught, so read it
// with that in mind.
//
// The search index and its versions (W110).
//
// `pages_fts` was created without naming a tokenizer, so it ran on the default
// `unicode61` WITHOUT diacritic folding. In German that costs more than it
// first sounds like:
//
//	Verträge   does not find   Vertrag
//	Straße     does not find   Strasse
//	Grüße      does not find   Gruesse
//
// With `remove_diacritics 2` SQLite folds ä→a, ö→o, ü→u BEFORE indexing.
// Together with the prefix search the query already appends, a large part of
// German inflection then falls away on its own: "Verträge" becomes "vertrage",
// and "vertrag*" reaches it.
//
// Why 2 and not 1: version 1 leaves ß and some eastern European characters
// alone; version 2 is the complete Unicode folding.
//
// A tokenizer cannot be changed on an existing FTS5 table — it has to be
// rebuilt and the content re-indexed. That happens once at startup, driven by
// a version number in app_settings.

// ftsVersion is the version this build expects. Raise it whenever the
// tokenizer line or the column layout changes.
const ftsVersion = "4"

// foldQuery folds a search term exactly the way the index does.
//
// Necessary because FTS5 applies the tokenizer to the STORED text, not to the
// MATCH pattern. Without this line somebody searches for "Verträge" while the
// index holds "vertrage" — and finds nothing.
//
// What `remove_diacritics 2` ACTUALLY does was checked against the index
// rather than assumed: ä→a, ü→u, é→e. The ß STAYS — it is not a diacritic but
// a letter of its own. The index holds "straßenbahn" and "gruße". Folding ß→ss
// here would search for "grusse" and never find anything; that is what the
// variant below is for.
func foldQuery(s string) string {
	r := strings.NewReplacer(
		"ä", "a", "Ä", "a", "ö", "o", "Ö", "o", "ü", "u", "Ü", "u",
		"é", "e", "è", "e", "ê", "e", "á", "a", "à", "a", "â", "a",
		"í", "i", "ì", "i", "ó", "o", "ò", "o", "ô", "o", "ú", "u", "ù", "u",
		"ç", "c", "ñ", "n", "å", "a", "ø", "o",
	)
	return r.Replace(strings.ToLower(s))
}

// German endings that get cut off when searching — longest first.
var germanSuffixes = []string{"ungen", "erin", "chen", "lein", "heit", "keit", "enen", "ern", "est", "end", "en", "er", "es", "em", "et", "e", "n", "s"}

// stemLite cuts off a common ending so the prefix search can bite.
//
// The case it is about: "Verträge" folds to "vertrage". As the prefix
// "vertrage*" that does NOT reach "Vertragsverlängerung" — which starts with
// "vertragsv". Only the stem "vertrag*" connects the two. In German, with its
// compounds, that is the difference between "finds the one page" and "finds
// everything on the subject".
//
// Deliberately cautious: only from six characters up, and the remainder has to
// keep at least four. Otherwise "Rate" turns into "Rat" and the search drags
// half the town hall along. The truncated stem does NOT replace the term, it
// joins it as an additional variant (see ftsMatch) — so a wrongly guessed stem
// loses nothing, it only adds noise that BM25 sorts to the back.
func stemLite(w string) string {
	if len([]rune(w)) < 6 {
		return w
	}
	for _, suf := range germanSuffixes {
		if strings.HasSuffix(w, suf) && len([]rune(w))-len([]rune(suf)) >= 4 {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

// ftsMatch builds the MATCH pattern out of an input.
//
// Each word yields up to three variants, joined by OR:
//
//	folded            "vertrage"*      — the term as the index writes it
//	stem              "vertrag"*       — connects inflection and compounding
//	ss/ß swap         "straße"*        — because the index keeps the ß while
//	                                     the keyboard often will not produce it
//
// The words stay AND-joined among themselves: whoever types two terms means
// both.
// indexable reports whether a word contains anything the tokenizer will keep.
//
// unicode61 treats letters and digits as token characters and EVERYTHING else
// as a separator. So a word made only of punctuation ("-", "/", "—") tokenizes
// to nothing, and `"-"*` becomes a legal but EMPTY phrase — which FTS5 then
// AND-s into the expression and matches nothing at all. One stray dash zeroed
// the whole query: `OMS - Trade` returned nothing while `OMS Trade` returned
// four pages, and copy-pasting a title (they are full of " - ") is the most
// ordinary thing a reader does.
func indexable(w string) bool {
	for _, r := range w {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func ftsMatch(q string) string {
	var groups []string
	for _, raw := range strings.Fields(foldQuery(q)) {
		// Dropped, not kept as an empty phrase: a separator carries no search
		// intent, and AND-ing it in destroys the terms that do.
		if !indexable(raw) {
			continue
		}
		seen := map[string]bool{}
		var alts []string
		add := func(t string) {
			t = strings.TrimSpace(t)
			if t == "" || seen[t] {
				return
			}
			seen[t] = true
			alts = append(alts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
		}
		add(raw)
		add(stemLite(raw))
		// Both directions: a typed "strasse" should find "straße" and the other
		// way round.
		if strings.Contains(raw, "ss") {
			add(strings.ReplaceAll(raw, "ss", "ß"))
		}
		if strings.Contains(raw, "ß") {
			add(strings.ReplaceAll(raw, "ß", "ss"))
		}
		if len(alts) == 1 {
			groups = append(groups, alts[0])
		} else {
			groups = append(groups, "("+strings.Join(alts, " OR ")+")")
		}
	}
	// Explicit "AND", not a bare space: FTS5's implicit-AND juxtaposition only
	// parses between two bare terms. The instant one word needs an OR-group
	// (its own stem/ß variant, added above), joining that group to a
	// neighbouring term with just whitespace is a syntax error — verified
	// against the live index, e.g. `("message"* OR "messag"*) "payload"*`
	// fails to parse while `("message"* OR "messag"*) AND "payload"*` runs
	// fine. Any multi-word query where at least one word is 6+ letters (stem
	// kicks in) or contains "ss"/"ß" hit this and silently produced zero
	// results — both searchChunks and its whole-page fallback share this same
	// match string, so there was no working fallback either.
	return strings.Join(groups, " AND ")
}

// migrateSearchIndex rebuilds the full-text index when it comes from an older
// version.
//
// The rebuild runs synchronously at startup: at 800 pages it takes a fraction
// of a second, and a half-migrated search would be worse than a slow start. On
// very large collections this is the place to move into the background later.
func (s *Server) migrateSearchIndex() error {
	if s.setting("fts_version", "1") == ftsVersion {
		return nil
	}
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS pages_fts`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM chunks_fts`); err != nil {
		return fmt.Errorf("clear chunks FTS: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM page_chunks`); err != nil {
		return fmt.Errorf("clear page chunks: %w", err)
	}
	if _, err := s.db.Exec(`CREATE VIRTUAL TABLE pages_fts USING fts5(
		id UNINDEXED, title, description, body,
		tokenize = "unicode61 remove_diacritics 2"
	)`); err != nil {
		return err
	}

	// Collect all page ids and index them ONLY AFTERWARDS: reindexPage issues
	// queries of its own, and on a single DB connection a query inside an open
	// cursor blocks the whole server.
	rows, err := s.db.Query(`SELECT id FROM pages WHERE trashed_at IS NULL`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("read pages for search index rebuild: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read pages for search index rebuild: %w", err)
	}
	rows.Close()

	failed := 0
	for _, id := range ids {
		if err := s.reindexPage(id); err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("search index rebuild: %d of %d pages could not be indexed", failed, len(ids))
	}
	if _, err := s.db.Exec(`INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, "fts_version", ftsVersion); err != nil {
		return fmt.Errorf("record search index version: %w", err)
	}
	log.Printf("search index: rebuilt (version %s, %d pages)", ftsVersion, len(ids)-failed)
	return nil
}
