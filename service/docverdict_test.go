package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// semML 的 CLIPTextEmbed 按提示词组返回可分辨的单位向量:文档组 → e0,照片组 → e1。
// 图向量由测试自行插入,贴近 e0 即语义文档、贴近 e1 即语义照片。
type semML struct{ mockML }

func (m *semML) CLIPTextEmbed(text string) ([]float32, error) {
	v := make([]float32, common.CLIPDim)
	for _, p := range photoPrompts {
		if p == text {
			v[1] = 1
			return v, nil
		}
	}
	v[0] = 1
	return v, nil
}

// 插入一个带图向量 + OCR 行的资产。vecDir=0 → 贴文档提示词,1 → 贴照片提示词。
func seedDocAsset(t *testing.T, ix *Indexer, id string, vecDir int, boxes [][]float64) {
	t.Helper()
	_, err := ix.db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
	require.NoError(t, err)
	_, err = ix.db.Exec(`INSERT INTO asset_ocr(asset_id,text,coverage,line_count,boxes_ver) VALUES(?, 'x', 0.1, ?, 1)`, id, len(boxes))
	require.NoError(t, err)
	for i, b := range boxes {
		blob, merr := json.Marshal(b)
		require.NoError(t, merr)
		_, err = ix.db.Exec(`INSERT INTO asset_ocr_lines(asset_id,line_no,text,box,score) VALUES(?,?,?,?,0.9)`, id, i, "line", string(blob))
		require.NoError(t, err)
	}
	vec := make([]float32, common.CLIPDim)
	vec[vecDir] = 1
	_, err = ix.db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
	require.NoError(t, err)
	var rowid int64
	require.NoError(t, ix.db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
	_, err = ix.db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
}

func TestComputeDocVerdict(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &semML{}, t.TempDir(), 1)

	seedDocAsset(t, ix, "doc1", 0, regularBoxes(10))   // 语义文档 + 几何规整
	seedDocAsset(t, ix, "photo1", 1, scatteredBoxes()) // 语义照片 + 几何散乱

	require.NoError(t, ix.computeDocVerdict("doc1"))
	require.NoError(t, ix.computeDocVerdict("photo1"))

	var isDoc, docVer int
	require.NoError(t, db.QueryRow(`SELECT is_doc, doc_ver FROM asset_ocr WHERE asset_id='doc1'`).Scan(&isDoc, &docVer))
	require.Equal(t, 1, isDoc)
	require.Equal(t, 1, docVer)
	require.NoError(t, db.QueryRow(`SELECT is_doc, doc_ver FROM asset_ocr WHERE asset_id='photo1'`).Scan(&isDoc, &docVer))
	require.Equal(t, 0, isDoc, "语义照片+几何散乱应被否决")

	// 提示词向量已进 DB 缓存
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM clip_text_cache WHERE gen=?`, common.MLModelGen).Scan(&n))
	require.Equal(t, len(docPrompts)+len(photoPrompts), n)
}

// offlineML:文本嵌入不可用(模拟 ML 离线)。
type offlineML struct{ mockML }

func (m *offlineML) CLIPTextEmbed(_ string) ([]float32, error) {
	return nil, errors.New("ml offline")
}
func (m *offlineML) IsReady() bool { return false }

func TestComputeDocVerdictMLOffline(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &offlineML{}, t.TempDir(), 1)
	seedDocAsset(t, ix, "a1", 0, regularBoxes(5))

	require.NoError(t, ix.computeDocVerdict("a1"), "ML 离线不算错误")

	var isDoc *int
	var docVer int
	var docGeo *float64
	require.NoError(t, db.QueryRow(`SELECT is_doc, doc_ver, doc_geo FROM asset_ocr WHERE asset_id='a1'`).Scan(&isDoc, &docVer, &docGeo))
	require.Nil(t, isDoc, "is_doc 留 NULL 等补算")
	require.Equal(t, 0, docVer, "doc_ver 留 0")
	require.NotNil(t, docGeo, "几何分不依赖 ML,先落库")
}

func TestComputeDocVerdictNoVector(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &semML{}, t.TempDir(), 1)
	// 只有 OCR 行、没有图向量
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text,coverage,line_count,boxes_ver) VALUES('a1','x',0.1,5,1)`)
	require.NoError(t, err)

	require.NoError(t, ix.computeDocVerdict("a1"))
	var docVer int
	require.NoError(t, db.QueryRow(`SELECT doc_ver FROM asset_ocr WHERE asset_id='a1'`).Scan(&docVer))
	require.Equal(t, 0, docVer, "无向量留待补算")
}
