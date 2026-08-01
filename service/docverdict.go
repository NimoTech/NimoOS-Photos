package service

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// readClipVector reads back the CLIP image vector for an asset_id; returns nil
// when there is no vector (not embedded / ScenesEnabled off). Same free-function
// style as dropClipVector.
func readClipVector(db *sql.DB, assetID string) []float32 {
	var rowid int64
	if db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&rowid) != nil {
		return nil
	}
	var blob []byte
	if db.QueryRow(`SELECT embedding FROM clip_embeddings WHERE rowid=?`, rowid).Scan(&blob) != nil {
		return nil
	}
	return sqlite.DeserializeFloat32(blob)
}

// loadPromptVecs returns the two prompt vector groups for doc/photo. Three-tier
// source: in-process cache → DB cache (clip_text_cache, keyed by MLModelGen) →
// CLIPTextEmbed (needs ML online, result written back to the DB). Failing to
// get a vector for any single prompt fails the whole call (an incomplete group
// would bias the margin).
func (ix *Indexer) loadPromptVecs() (docVecs, photoVecs [][]float32, err error) {
	ix.promptMu.Lock()
	defer ix.promptMu.Unlock()
	if ix.promptDoc != nil && ix.promptPhoto != nil {
		return ix.promptDoc, ix.promptPhoto, nil
	}

	fetch := func(prompt string) ([]float32, error) {
		var blob []byte
		err := ix.db.QueryRow(
			`SELECT vec FROM clip_text_cache WHERE key=? AND gen=?`,
			prompt, common.MLModelGen).Scan(&blob)
		if err == nil {
			if v := sqlite.DeserializeFloat32(blob); v != nil {
				return v, nil
			}
		}
		v, err := ix.ml.CLIPTextEmbed(prompt)
		if err != nil {
			return nil, err
		}
		_, _ = ix.db.Exec(
			`INSERT INTO clip_text_cache(key, gen, vec) VALUES(?,?,?)
			 ON CONFLICT(key, gen) DO UPDATE SET vec=excluded.vec`,
			prompt, common.MLModelGen, sqlite.SerializeFloat32(v))
		return v, nil
	}

	doc := make([][]float32, 0, len(docPrompts))
	for _, p := range docPrompts {
		v, ferr := fetch(p)
		if ferr != nil {
			return nil, nil, fmt.Errorf("loadPromptVecs %q: %w", p, ferr)
		}
		doc = append(doc, v)
	}
	photo := make([][]float32, 0, len(photoPrompts))
	for _, p := range photoPrompts {
		v, ferr := fetch(p)
		if ferr != nil {
			return nil, nil, fmt.Errorf("loadPromptVecs %q: %w", p, ferr)
		}
		photo = append(photo, v)
	}
	ix.promptDoc, ix.promptPhoto = doc, photo
	return doc, photo, nil
}

// computeDocVerdict computes and persists the hybrid doc verdict for an asset
// that has finished OCR (boxes_ver=1). Pure local math plus a one-time prompt
// text embedding; no image inference runs here.
// Degraded paths (both return nil, leaving doc_ver=0 for later backfill): the
// image vector is missing; the prompt vectors are unavailable (ML offline). In
// both cases the geometry score is persisted first (it doesn't depend on ML),
// for observability.
func (ix *Indexer) computeDocVerdict(assetID string) error {
	rows, err := ix.db.Query(
		`SELECT box FROM asset_ocr_lines WHERE asset_id=? ORDER BY line_no`, assetID)
	if err != nil {
		return err
	}
	boxes := make([][]float64, 0, 8)
	for rows.Next() {
		var boxJSON string
		if err := rows.Scan(&boxJSON); err != nil {
			rows.Close()
			return err
		}
		var b []float64
		if json.Unmarshal([]byte(boxJSON), &b) == nil && len(b) == 8 {
			boxes = append(boxes, b)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	geo := docGeoScore(boxes)

	imgVec := readClipVector(ix.db, assetID)
	if imgVec == nil {
		_, err := ix.db.Exec(`UPDATE asset_ocr SET doc_geo=? WHERE asset_id=?`, geo, assetID)
		return err
	}
	docVecs, photoVecs, err := ix.loadPromptVecs()
	if err != nil {
		// ML offline, etc.: persist the geometry score first, verdict computed later.
		_, uerr := ix.db.Exec(`UPDATE asset_ocr SET doc_geo=? WHERE asset_id=?`, geo, assetID)
		if uerr != nil {
			return uerr
		}
		return nil
	}

	sem := docSemMargin(imgVec, docVecs, photoVecs)
	isDoc := 0
	if docVerdict(sem, geo) {
		isDoc = 1
	}
	_, err = ix.db.Exec(
		`UPDATE asset_ocr SET doc_sem=?, doc_geo=?, is_doc=?, doc_ver=1 WHERE asset_id=?`,
		sem, geo, isDoc, assetID)
	return err
}
