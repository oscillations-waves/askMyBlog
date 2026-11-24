package db

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d
}

func storedRows(t *testing.T, d *DB, table string) int {
	t.Helper()
	stmt, _, err := d.conn.Prepare("SELECT COUNT(*) FROM " + table)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if !stmt.Step() {
		t.Fatalf("count %s: %v", table, stmt.Err())
	}
	return stmt.ColumnInt(0)
}

func TestInsertInvalidEmbeddingLeavesNoChunk(t *testing.T) {
	d := openTestDB(t)
	_, err := d.Insert(Chunk{Slug: "invalid", Content: "must not persist"}, []float32{1, 2})
	if err == nil {
		t.Fatal("Insert accepted a 2-dimensional embedding")
	}
	if !strings.Contains(err.Error(), "768") {
		t.Errorf("error does not explain required dimensions: %v", err)
	}
	if got := storedRows(t, d, "chunks"); got != 0 {
		t.Errorf("invalid embedding left %d orphan chunks", got)
	}
}

func TestInsertVectorFailureRollsBackChunk(t *testing.T) {
	d := openTestDB(t)
	if err := d.conn.Exec(`DROP TABLE vec_chunks`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Insert(Chunk{Slug: "failed", Content: "must not persist"}, make([]float32, 768)); err == nil {
		t.Fatal("Insert succeeded without the vector table")
	}
	if got := storedRows(t, d, "chunks"); got != 0 {
		t.Errorf("vector SQL failure left %d orphan chunks", got)
	}
}

func TestClearSlugRollsBackWhenChunkDeletionFails(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.Insert(Chunk{Slug: "keep", Content: "searchable"}, make([]float32, 768)); err != nil {
		t.Fatal(err)
	}
	if err := d.conn.Exec(`CREATE TRIGGER reject_chunk_delete BEFORE DELETE ON chunks
		BEGIN SELECT RAISE(ABORT, 'reject delete'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.ClearSlug("keep"); err == nil {
		t.Fatal("ClearSlug ignored the deletion failure")
	}
	if got := storedRows(t, d, "chunks"); got != 1 {
		t.Errorf("chunks after failed deletion = %d, want 1", got)
	}
	if got := storedRows(t, d, "vec_chunks"); got != 1 {
		t.Errorf("vectors after failed deletion = %d, want 1", got)
	}
}

func TestCountIncludesOnlySearchableChunks(t *testing.T) {
	d := openTestDB(t)
	if got, err := d.Count(); err != nil || got != 0 {
		t.Fatalf("empty Count = %d, %v; want 0, nil", got, err)
	}
	for _, slug := range []string{"first", "second"} {
		if _, err := d.Insert(Chunk{Slug: slug}, make([]float32, 768)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.conn.Exec(`INSERT INTO chunks (slug, title, date, tags, chunk_idx, content)
		VALUES ('orphan', '', '', '', 0, '')`); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Count(); err != nil || got != 2 {
		t.Fatalf("Count = %d, %v; want 2 searchable chunks", got, err)
	}
	if err := d.ClearSlug("first"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Count(); err != nil || got != 1 {
		t.Fatalf("Count after ClearSlug = %d, %v; want 1, nil", got, err)
	}
}

func TestConcurrentSearch(t *testing.T) {
	d := openTestDB(t)
	embedding := make([]float32, 768)
	id, err := d.Insert(Chunk{Slug: "post", Content: "one matching chunk"}, embedding)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make(chan error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					errs <- fmt.Errorf("concurrent Search panicked: %v", p)
				}
			}()
			<-start
			for range 10 {
				results, err := d.Search(embedding, 1)
				if err != nil {
					errs <- err
					return
				}
				if len(results) != 1 || results[0].ID != id || results[0].Content != "one matching chunk" {
					errs <- fmt.Errorf("concurrent Search returned unexpected results: %+v", results)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
