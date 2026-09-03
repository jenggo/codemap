package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"codemap/chunk"
)

// ErrUnknownResponse reports that a response_id was never stored or has since
// been pruned, so callers can distinguish a bad reference from a query
// failure (the same distinction the query package makes with ErrNoPath).
var ErrUnknownResponse = errors.New("unknown response_id")

// responseChunkCapBytes bounds the total stored overflow bytes. The oldest
// responses (by created_at) are evicted whole, one response_id group at a
// time, until the store fits the cap.
var responseChunkCapBytes = 4 << 20

// ResponseSection is one stored section of a gated oversized tool response.
type ResponseSection struct {
	ResponseID string
	Tool       string
	Title      string
	Content    string
	CreatedAt  string
	SectionIdx int
	ByteCount  int
	// Score is the FTS5 bm25 rank for search results (lower is better) and
	// 0 for plain reads.
	Score float64
}

// ResponseGroup is the grouped metadata describing one stored response.
// Manifest fields are derived from this grouped query, not a second table.
type ResponseGroup struct {
	ResponseID string
	Tool       string
	CreatedAt  string
	Sections   int
	TotalBytes int
}

// SaveResponseChunks stores every section of one gated response under a
// fresh response_id and returns it. The insert runs an LRU prune that evicts
// the oldest stored responses until total overflow bytes fit the cap; the
// response being saved is never evicted.
func (s *Store) SaveResponseChunks(tool string, sections []chunk.Section) (string, error) {
	if len(sections) == 0 {
		return "", errors.New("no response sections to store")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	id := newResponseID(tool, now)
	createdAt := now.Format(time.RFC3339Nano)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for i, sec := range sections {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO response_chunks (response_id, tool, section_idx, title, content, byte_count, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, tool, i, sec.Title, sec.Content, len(sec.Content), createdAt)
			if err != nil {
				return fmt.Errorf("save response chunk %d: %w", i, err)
			}
		}
		return pruneResponseChunksToCap(ctx, tx, id)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// newResponseID derives a unique id from a monotonic nanosecond prefix and a
// short hash of the tool name, so ids sort by creation time and never
// collide across gated responses.
func newResponseID(tool string, now time.Time) string {
	nano := now.UnixNano()
	h := fnv.New64a()
	_, _ = h.Write([]byte(tool))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatInt(nano, 10)))
	return fmt.Sprintf("resp-%020d-%016x", nano, h.Sum64())
}

// pruneResponseChunksToCap evicts whole response groups oldest-first until
// the total stored bytes fit the cap. It stops early when only the response
// being saved remains, so a single oversized response is kept whole.
func pruneResponseChunksToCap(ctx context.Context, tx *sql.Tx, keepID string) error {
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(byte_count), 0) FROM response_chunks`).Scan(&total); err != nil {
		return err
	}
	for total > int64(responseChunkCapBytes) {
		var oldest string
		err := tx.QueryRowContext(ctx, `
			SELECT response_id FROM response_chunks
			WHERE response_id <> ?
			ORDER BY created_at, rowid
			LIMIT 1`, keepID).Scan(&oldest)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM response_chunks WHERE response_id = ?`, oldest); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(byte_count), 0) FROM response_chunks`).Scan(&total); err != nil {
			return err
		}
	}
	return nil
}

// ResponseManifest returns the grouped metadata for one stored response.
func (s *Store) ResponseManifest(responseID string) (ResponseGroup, error) {
	const q = `
		SELECT response_id, tool, COUNT(*), COALESCE(SUM(byte_count), 0), MIN(created_at)
		FROM response_chunks
		WHERE response_id = ?
		GROUP BY response_id, tool`
	var g ResponseGroup
	err := s.db.QueryRowContext(context.Background(), q, responseID).Scan(&g.ResponseID, &g.Tool, &g.Sections, &g.TotalBytes, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ResponseGroup{}, fmt.Errorf("%w: %s", ErrUnknownResponse, responseID)
	}
	if err != nil {
		return ResponseGroup{}, err
	}
	return g, nil
}

// ResponseSections returns the stored sections of one response, ordered by
// section index. A nil sectionIdx returns every section; a non-nil index
// returns at most that one section (empty when the index is out of range).
// An unknown response_id fails with ErrUnknownResponse.
func (s *Store) ResponseSections(responseID string, sectionIdx *int) ([]ResponseSection, error) {
	exists, err := s.responseExists(responseID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrUnknownResponse, responseID)
	}

	q := `SELECT response_id, tool, section_idx, title, content, byte_count, created_at, 0
		FROM response_chunks WHERE response_id = ?`
	args := []any{responseID}
	if sectionIdx != nil {
		q += ` AND section_idx = ?`
		args = append(args, *sectionIdx)
	}
	q += ` ORDER BY section_idx`
	return s.queryResponseSections(q, args...)
}

// SearchResponseSections returns the sections of one response matching the
// keyword query, ranked by full-text relevance and capped at limit. The
// trigram tokenizer matches identifiers inside section bodies; patterns too
// short for the tokenizer fall back to a verbatim substring scan. An unknown
// response_id fails with ErrUnknownResponse.
func (s *Store) SearchResponseSections(responseID, pattern string, limit int) ([]ResponseSection, error) {
	if limit <= 0 {
		limit = 5
	}
	exists, err := s.responseExists(responseID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrUnknownResponse, responseID)
	}

	sanitized := sanitizeFTSQuery(pattern)
	if sanitized == "" {
		return nil, nil
	}

	var q string
	var args []any
	if ftsNeedsFullScan(pattern) {
		// The trigram tokenizer only serves terms of at least three
		// characters; a shorter term silently matches nothing in MATCH, so
		// scan the content directly instead.
		q = `SELECT response_id, tool, section_idx, title, content, byte_count, created_at, 0
			FROM response_chunks
			WHERE response_id = ? AND instr(content, ?) > 0
			ORDER BY section_idx
			LIMIT ?`
		args = []any{responseID, pattern, limit}
	} else {
		q = `SELECT response_id, tool, section_idx, title, content, byte_count, created_at, rank
			FROM response_chunks
			WHERE response_chunks MATCH ? AND response_id = ?
			ORDER BY rank, section_idx
			LIMIT ?`
		args = []any{sanitized, responseID, limit}
	}
	return s.queryResponseSections(q, args...)
}

// PruneResponseChunks deletes every stored response chunk. Re-index paths
// call this (through their own transactions) because overflow data is a
// cache that never outlives the index generation it was captured against.
func (s *Store) PruneResponseChunks() error {
	ctx := context.Background()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return deleteResponseChunksTx(ctx, tx)
	})
}

// deleteResponseChunksTx removes every response chunk inside the caller's
// transaction, so a successful index write and its overflow prune commit or
// roll back together.
func deleteResponseChunksTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM response_chunks`)
	if err != nil {
		return fmt.Errorf("prune response chunks: %w", err)
	}
	return nil
}

func (s *Store) responseExists(responseID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM response_chunks WHERE response_id = ?`, responseID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) queryResponseSections(q string, args ...any) ([]ResponseSection, error) {
	rows, err := s.db.QueryContext(context.Background(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("response section query failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sections []ResponseSection
	for rows.Next() {
		var sec ResponseSection
		if err := rows.Scan(&sec.ResponseID, &sec.Tool, &sec.SectionIdx, &sec.Title,
			&sec.Content, &sec.ByteCount, &sec.CreatedAt, &sec.Score); err != nil {
			return nil, err
		}
		sections = append(sections, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sections, nil
}
