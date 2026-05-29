// Package db wraps sqlite + sqlite-vec for chunk storage and similarity search.
package db

import (
	"encoding/binary"
	"fmt"
	"math"

	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces" // provides sqlite3.Binary with vec0 built in
	"github.com/ncruces/go-sqlite3"
)

// Chunk is a row in the `chunks` table.
type Chunk struct {
	ID         int64
	Slug       string
	Title      string
	Date       string
	Tags       string
	ChunkIndex int
	Content    string
}

// Result is a Chunk with its distance from the query embedding.
type Result struct {
	Chunk
	Distance float64
}

// DB owns a single sqlite connection with sqlite-vec loaded.
type DB struct {
	conn *sqlite3.Conn
}

// Open opens (or creates) the database at path and ensures the schema exists.
func Open(path string) (*DB, error) {
	conn, err := sqlite3.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	const schema = `
		CREATE TABLE IF NOT EXISTS chunks (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			slug      TEXT NOT NULL,
			title     TEXT NOT NULL,
			date      TEXT NOT NULL,
			tags      TEXT NOT NULL,
			chunk_idx INTEGER NOT NULL,
			content   TEXT NOT NULL
		);
		CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(
			embedding float[768]
		);
	`
	if err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &DB{conn: conn}, nil
}

// Close releases the underlying connection.
func (d *DB) Close() error { return d.conn.Close() }

// ClearSlug removes all chunks (and their vectors) for a given slug.
func (d *DB) ClearSlug(slug string) error {
	stmt, _, err := d.conn.Prepare(`SELECT id FROM chunks WHERE slug = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	stmt.BindText(1, slug)

	var ids []int64
	for stmt.Step() {
		ids = append(ids, stmt.ColumnInt64(0))
	}
	if err := stmt.Err(); err != nil {
		return err
	}

	for _, id := range ids {
		delVec, _, err := d.conn.Prepare(`DELETE FROM vec_chunks WHERE rowid = ?`)
		if err != nil {
			return err
		}
		delVec.BindInt64(1, id)
		if err := delVec.Exec(); err != nil {
			delVec.Close()
			return err
		}
		delVec.Close()
	}
	delChunks, _, err := d.conn.Prepare(`DELETE FROM chunks WHERE slug = ?`)
	if err != nil {
		return err
	}
	defer delChunks.Close()
	delChunks.BindText(1, slug)
	return delChunks.Exec()
}

// Insert writes a chunk and its embedding atomically.
func (d *DB) Insert(c Chunk, embedding []float32) (int64, error) {
	ins, _, err := d.conn.Prepare(
		`INSERT INTO chunks (slug, title, date, tags, chunk_idx, content)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	ins.BindText(1, c.Slug)
	ins.BindText(2, c.Title)
	ins.BindText(3, c.Date)
	ins.BindText(4, c.Tags)
	ins.BindInt(5, c.ChunkIndex)
	ins.BindText(6, c.Content)
	if err := ins.Exec(); err != nil {
		return 0, err
	}
	id := d.conn.LastInsertRowID()

	vec, _, err := d.conn.Prepare(`INSERT INTO vec_chunks(rowid, embedding) VALUES (?, ?)`)
	if err != nil {
		return 0, err
	}
	defer vec.Close()
	vec.BindInt64(1, id)
	vec.BindBlob(2, float32ToBytes(embedding))
	if err := vec.Exec(); err != nil {
		return 0, err
	}
	return id, nil
}

// Search returns the k nearest chunks to the query embedding.
func (d *DB) Search(query []float32, k int) ([]Result, error) {
	stmt, _, err := d.conn.Prepare(
		`SELECT rowid, distance FROM vec_chunks
		 WHERE embedding MATCH ? ORDER BY distance LIMIT ?`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	stmt.BindBlob(1, float32ToBytes(query))
	stmt.BindInt(2, k)

	type hit struct {
		id   int64
		dist float64
	}
	var hits []hit
	for stmt.Step() {
		hits = append(hits, hit{id: stmt.ColumnInt64(0), dist: stmt.ColumnFloat(1)})
	}
	if err := stmt.Err(); err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(hits))
	for _, h := range hits {
		c, err := d.getByID(h.id)
		if err != nil {
			return nil, err
		}
		results = append(results, Result{Chunk: c, Distance: h.dist})
	}
	return results, nil
}

func (d *DB) getByID(id int64) (Chunk, error) {
	stmt, _, err := d.conn.Prepare(
		`SELECT id, slug, title, date, tags, chunk_idx, content FROM chunks WHERE id = ?`)
	if err != nil {
		return Chunk{}, err
	}
	defer stmt.Close()
	stmt.BindInt64(1, id)
	if !stmt.Step() {
		return Chunk{}, fmt.Errorf("chunk %d not found", id)
	}
	return Chunk{
		ID:         stmt.ColumnInt64(0),
		Slug:       stmt.ColumnText(1),
		Title:      stmt.ColumnText(2),
		Date:       stmt.ColumnText(3),
		Tags:       stmt.ColumnText(4),
		ChunkIndex: stmt.ColumnInt(5),
		Content:    stmt.ColumnText(6),
	}, nil
}

func float32ToBytes(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}
