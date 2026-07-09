package service

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// readClipVector 按 asset_id 读回 CLIP 图向量;无向量(未嵌入/ScenesEnabled 关)
// 返回 nil。与 dropClipVector 同款自由函数风格。
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

// loadPromptVecs 返回文档/照片两组提示词向量。三级来源:进程内缓存 → DB 缓存
// (clip_text_cache,按 MLModelGen 键)→ CLIPTextEmbed(需 ML 在线,结果写回 DB)。
// 任一提示词拿不到向量即整体失败(组不完整会使边际有偏)。
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

// computeDocVerdict 为一个已完成 OCR(boxes_ver=1)的资产计算 doc 混合判定并写库。
// 纯本地数学 + 首次的提示词文本嵌入;不跑图像推理。
// 降级路径(均返回 nil,留 doc_ver=0 等补算):图向量缺失;提示词向量不可得(ML 离线)。
// 两种情况下几何分先落库(不依赖 ML),便于观测。
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
		// ML 离线等:几何分先落库,判定等补算。
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
