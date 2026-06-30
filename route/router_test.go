package route

import (
	"net/http"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
)

func TestMcpReadSkip(t *testing.T) {
	base := common.V1APIPath
	cases := []struct {
		name                       string
		method, path, ip, userID   string
		want                       bool
	}{
		{"albums GET localhost with uid", http.MethodGet, base + "/albums", "127.0.0.1", "42", true},
		{"albums GET localhost no uid (fail-closed)", http.MethodGet, base + "/albums", "127.0.0.1", "", false},
		{"albums POST localhost (write stays protected)", http.MethodPost, base + "/albums", "127.0.0.1", "42", false},
		{"albums GET external ip", http.MethodGet, base + "/albums", "192.168.1.5", "42", false},
		{"search/smart POST localhost with uid", http.MethodPost, base + "/search/smart", "127.0.0.1", "42", true},
		{"suffix-lookalike not exact-matched", http.MethodGet, base + "/public/albums", "127.0.0.1", "42", false},
	}
	for _, c := range cases {
		if got := mcpReadSkip(c.method, c.path, c.ip, c.userID); got != c.want {
			t.Errorf("%s: mcpReadSkip=%v want %v", c.name, got, c.want)
		}
	}
}
