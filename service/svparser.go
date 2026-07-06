package service

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParsedCond is a single Smart View condition resolved into an executable form.
type ParsedCond struct {
	Raw   string     `json:"raw"`
	Kind  string     `json:"kind"`
	Value string     `json:"value"`
	Start *time.Time `json:"start,omitempty"`
	End   *time.Time `json:"end,omitempty"`
}

const (
	condPerson      = "person"
	condPlace       = "place"
	condDate        = "date"
	condSemantic    = "semantic"
	condOCR         = "ocr"
	condUnsupported = "unsupported"
)

var unsupportedPrefixes = []string{
	"type:", "gps:", "group:", "faces:", "time:", "event:",
	"older than", "amount detected",
}

// scoreCondRe：score 后跟（可选空白 +）比较符的任意变体，或裸数字（无比较符，
// 如 "score 80"）。之前用前缀字面量枚举（"score "/"score≥"/"score >="），漏掉了
// 无空格的 "score>=80" 这类写法；改成比较符正则后又丢了旧版能拦的裸数字写法
// "score 80"——两者都要拦，同时保留 "score of the game" 这类正常语义词不误伤。
var scoreCondRe = regexp.MustCompile(`^score(\s*(>=|<=|==|=|≥|≤|>|<)|\s+\d)`)

var (
	// "year: 2024" 显式前缀，或裸年份 "2024"（与搜索链路的
	// "Nimo understood: date" 行为一致——用户直接把年份当 chip 很常见）
	reYear      = regexp.MustCompile(`(?i)^(?:year:\s*)?((?:19|20)\d{2})$`)
	reLastNDays = regexp.MustCompile(`(?i)^(?:captured:\s*)?last\s+(\d+)\s+days$`)
	reMonthSpan = regexp.MustCompile(`(?i)^([a-z]{3})\s+(\d{1,2})\s*[–\-]\s*(\d{1,2}),\s*(\d{4})$`)
	reMonthYear = regexp.MustCompile(`(?i)^([a-z]{3})\s+(\d{4})\s*[–\-]\s*([a-z]{3})\s+(\d{4})$`)
)

var monthAbbr = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
	"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
	"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
}

func ParseConditions(db *sql.DB, raw []string) []ParsedCond {
	return parseConditionsAt(db, raw, time.Now())
}

func parseConditionsAt(db *sql.DB, raw []string, now time.Time) []ParsedCond {
	out := make([]ParsedCond, 0, len(raw))
	for _, r := range raw {
		out = append(out, parseOne(db, strings.TrimSpace(r), now))
	}
	return out
}

func parseOne(db *sql.DB, r string, now time.Time) ParsedCond {
	low := strings.ToLower(r)
	if scoreCondRe.MatchString(low) {
		return ParsedCond{Raw: r, Kind: condUnsupported, Value: r}
	}
	for _, p := range unsupportedPrefixes {
		if strings.HasPrefix(low, p) {
			return ParsedCond{Raw: r, Kind: condUnsupported, Value: r}
		}
	}
	if c, ok := parseDate(r, now); ok {
		c.Raw = r
		return c
	}
	if strings.HasPrefix(low, "scene:") || strings.HasPrefix(low, "object:") {
		q := strings.TrimSpace(r[strings.Index(r, ":")+1:])
		return ParsedCond{Raw: r, Kind: condSemantic, Value: q}
	}
	// "ocr: receipt | invoice" — substring match against recognized text;
	// "|" separates OR alternatives (split at evaluation time).
	if strings.HasPrefix(low, "ocr:") {
		q := strings.TrimSpace(r[strings.Index(r, ":")+1:])
		return ParsedCond{Raw: r, Kind: condOCR, Value: q}
	}
	if strings.HasPrefix(low, "place:") {
		v := strings.TrimSpace(r[strings.Index(r, ":")+1:])
		return ParsedCond{Raw: r, Kind: condPlace, Value: v}
	}
	if id, ok := lookupPersonID(db, r); ok {
		return ParsedCond{Raw: r, Kind: condPerson, Value: id}
	}
	if v, ok := lookupPlace(db, r); ok {
		return ParsedCond{Raw: r, Kind: condPlace, Value: v}
	}
	return ParsedCond{Raw: r, Kind: condSemantic, Value: r}
}

func parseDate(r string, now time.Time) (ParsedCond, bool) {
	if m := reYear.FindStringSubmatch(r); m != nil {
		y, _ := strconv.Atoi(m[1])
		s := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		e := time.Date(y, 12, 31, 23, 59, 59, 0, time.UTC)
		return ParsedCond{Kind: condDate, Start: &s, End: &e}, true
	}
	if m := reLastNDays.FindStringSubmatch(r); m != nil {
		n, _ := strconv.Atoi(m[1])
		s := now.AddDate(0, 0, -n)
		e := now
		return ParsedCond{Kind: condDate, Start: &s, End: &e}, true
	}
	if m := reMonthSpan.FindStringSubmatch(r); m != nil {
		mon, ok := monthAbbr[strings.ToLower(m[1])]
		if ok {
			d1, _ := strconv.Atoi(m[2])
			d2, _ := strconv.Atoi(m[3])
			y, _ := strconv.Atoi(m[4])
			s := time.Date(y, mon, d1, 0, 0, 0, 0, time.UTC)
			e := time.Date(y, mon, d2, 23, 59, 59, 0, time.UTC)
			return ParsedCond{Kind: condDate, Start: &s, End: &e}, true
		}
	}
	if m := reMonthYear.FindStringSubmatch(r); m != nil {
		m1, ok1 := monthAbbr[strings.ToLower(m[1])]
		m2, ok2 := monthAbbr[strings.ToLower(m[3])]
		if ok1 && ok2 {
			y1, _ := strconv.Atoi(m[2])
			y2, _ := strconv.Atoi(m[4])
			s := time.Date(y1, m1, 1, 0, 0, 0, 0, time.UTC)
			e := time.Date(y2, m2+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
			return ParsedCond{Kind: condDate, Start: &s, End: &e}, true
		}
	}
	return ParsedCond{}, false
}

func lookupPersonID(db *sql.DB, name string) (string, bool) {
	var id string
	err := db.QueryRow(`SELECT id FROM persons WHERE lower(name)=lower(?) LIMIT 1`, name).Scan(&id)
	if err != nil {
		return "", false
	}
	return id, true
}

func lookupPlace(db *sql.DB, r string) (string, bool) {
	candidates := []string{r}
	if i := strings.Index(r, ","); i > 0 {
		candidates = append(candidates, strings.TrimSpace(r[:i]), strings.TrimSpace(r[i+1:]))
	}
	for _, cand := range candidates {
		var hit int
		err := db.QueryRow(`SELECT COUNT(*) FROM asset_geo
			WHERE lower(city)=lower(?) OR lower(country)=lower(?) OR lower(region)=lower(?)`,
			cand, cand, cand).Scan(&hit)
		if err == nil && hit > 0 {
			return cand, true
		}
	}
	return "", false
}
