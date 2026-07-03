package service

import "testing"

// TestExpandQuery 验证短单词式查询被模板化为句子（CLIP 文本编码器句子导向，
// 单词式短查询判别力差），而已经"像句子"的查询保持原样，避免重复包裹模板。
func TestExpandQuery(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"英文单词", "kid", "a photo of kid"},
		{"英文两词", "little kid", "a photo of little kid"},
		{"长英文句不变", "a happy kid playing in the park", "a happy kid playing in the park"},
		{"已含photo of不重复包", "photo of cat", "photo of cat"},
		{"已含photo of大小写不敏感", "Photo Of cat", "Photo Of cat"},
		{"中文双字", "小孩", "一张小孩的照片"},
		{"中文长句不变", "一个可爱的小孩在公园里玩耍", "一个可爱的小孩在公园里玩耍"},
		{"已含照片不重复包", "猫照片", "猫照片"},
		{"空串", "", ""},
		{"空白串", "   ", "   "},
		{"混合中英短查询走中文模板", "AI猫", "一张AI猫的照片"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := expandQuery(c.in)
			if got != c.want {
				t.Errorf("expandQuery(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
