#!/usr/bin/env python3
"""美学分探针验收报告:取全库最高/最低各 N 张,生成本地 HTML 对比页。
用法: python3 report.py --db /DATA/.system_data/photos/photos.db \
                        --thumbs /DATA/.system_data/photos/thumbs \
                        --out /tmp/aesthetic-report.html --n 30
"""
import argparse
import html
import sqlite3

def rows(db, order, n):
    return db.execute(
        "SELECT id, file_path, aesthetic_score FROM assets "
        "WHERE aesthetic_score IS NOT NULL AND deleted_at IS NULL "
        f"ORDER BY aesthetic_score {order} LIMIT ?", (n,)).fetchall()

def section(title, items, thumbs):
    cells = "".join(
        f'<figure><img src="file://{thumbs}/{i}/small.jpg" loading="lazy">'
        f"<figcaption>{s:.3f}<br><small>{html.escape(p.rsplit('/',1)[-1])}</small></figcaption></figure>"
        for i, p, s in items)
    return f"<h2>{title}</h2><div class='grid'>{cells}</div>"

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True)
    ap.add_argument("--thumbs", required=True)
    ap.add_argument("--out", default="/tmp/aesthetic-report.html")
    ap.add_argument("--n", type=int, default=30)
    a = ap.parse_args()
    db = sqlite3.connect(f"file:{a.db}?mode=ro", uri=True)
    top, bottom = rows(db, "DESC", a.n), rows(db, "ASC", a.n)
    total = db.execute("SELECT COUNT(*) FROM assets WHERE aesthetic_score IS NOT NULL").fetchone()[0]
    page = ("<meta charset='utf-8'><title>美学分探针验收</title>"
            "<style>.grid{display:grid;grid-template-columns:repeat(6,1fr);gap:8px}"
            "img{width:100%;aspect-ratio:1;object-fit:cover}figure{margin:0;font:12px sans-serif}</style>"
            f"<h1>美学分探针验收(已打分 {total} 张)</h1>"
            + section("最高分", top, a.thumbs) + section("最低分", bottom, a.thumbs))
    with open(a.out, "w") as f:
        f.write(page)
    print(f"OK -> {a.out}")

if __name__ == "__main__":
    main()
