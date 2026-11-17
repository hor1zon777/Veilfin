package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type tagStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func tagCounts(u *user, kind string) map[string]int {
	var list []tagStat
	u.must("admin.tags.list", map[string]any{"kind": kind}, &list)
	out := map[string]int{}
	for _, t := range list {
		out[t.Name] = t.Count
	}
	return out
}

// TestGenreManage：类型的列表、重命名、合并、删除，以及改过的类型不会被重新刮削覆盖。
func TestGenreManage(t *testing.T) {
	if !hasFFmpeg() {
		t.Skip("需要 ffmpeg")
	}
	e := newEnv(t)
	e.useFakeScrapers(fakeMetaTube(t), fakeTMDB(t))
	home := filepath.Join(e.mediaDir, "home")
	movies := filepath.Join(e.mediaDir, "movies")
	nfo := func(name, title string, genres ...string) {
		makeVideo(t, filepath.Join(home, name+".mp4"), 2, 440)
		x := "<movie><title>" + title + "</title>"
		for _, g := range genres {
			x += "<genre>" + g + "</genre>"
		}
		os.WriteFile(filepath.Join(home, name+".nfo"), []byte(x+"</movie>"), 0o644)
	}
	nfo("a", "甲", "剧情", "Drama", "家庭")
	nfo("b", "乙", "家庭", "Drama")
	nfo("c", "丙", "Drama")
	makeVideo(t, filepath.Join(movies, "The.Matrix.1999.mp4"), 2, 550)

	admin := e.setupAdmin()
	admin.must("admin.libs.create", map[string]any{"name": "家庭录像", "kind": "home", "scraper": "none", "paths": []string{home}}, nil)
	admin.must("admin.libs.create", map[string]any{"name": "电影", "kind": "movie", "scraper": "tmdb", "paths": []string{movies}}, nil)
	e.waitIdle()

	if got, want := tagCounts(admin, "genre"), map[string]int{"剧情": 1, "Drama": 3, "家庭": 2, "动作": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("类型统计不对: %v", got)
	}
	var all listResult
	admin.must("items.list", map[string]any{"libraryId": 0, "limit": 100}, &all)
	genres := func(title string) (map[string][]string, []string) {
		var d detail
		admin.must("items.get", map[string]any{"id": findByTitle(t, all.Items, title).ID}, &d)
		return d.Tags, d.Locked
	}

	// 合并：Drama 并入已有的 剧情；甲同时有两者，合并后只剩一个，且保持原有顺序
	var r struct {
		Items int `json:"items"`
	}
	admin.must("admin.tags.rename", map[string]any{"kind": "genre", "from": []string{"Drama"}, "to": " 剧情 "}, &r)
	if r.Items != 3 {
		t.Fatalf("合并应影响 3 个条目，实际 %d", r.Items)
	}
	if got := tagCounts(admin, "genre"); got["剧情"] != 3 || got["Drama"] != 0 {
		t.Fatalf("合并后统计不对: %v", got)
	}
	if g, locked := genres("甲"); !reflect.DeepEqual(g["genre"], []string{"剧情", "家庭"}) || !contains(locked, "genres") {
		t.Fatalf("甲的类型不对: %v，锁定 %v", g["genre"], locked)
	}
	if g, _ := genres("乙"); !reflect.DeepEqual(g["genre"], []string{"家庭", "剧情"}) {
		t.Fatalf("改名应保留原来的顺序: %v", g["genre"])
	}

	// 删除
	admin.must("admin.tags.delete", map[string]any{"kind": "genre", "names": []string{"家庭"}}, &r)
	if got := tagCounts(admin, "genre"); r.Items != 2 || got["家庭"] != 0 || got["剧情"] != 3 {
		t.Fatalf("删除后统计不对: %v（影响 %d）", got, r.Items)
	}

	// 改过的类型被锁定，重新刮削不会改回去
	admin.must("admin.tags.rename", map[string]any{"kind": "genre", "from": []string{"动作"}, "to": "Action"}, nil)
	matrix := findByTitle(t, all.Items, "黑客帝国")
	admin.must("items.rescrape", map[string]any{"id": matrix.ID}, nil)
	if g, _ := genres("黑客帝国"); !reflect.DeepEqual(g["genre"], []string{"Action"}) || !reflect.DeepEqual(g["actor"], []string{"基努·里维斯"}) {
		t.Fatalf("重新刮削覆盖了改过的类型，或影响了其他字段: %v", g)
	}

	// 参数校验与权限
	admin.expectCode("admin.tags.list", map[string]any{"kind": "studio"}, "bad_request")
	admin.expectCode("admin.tags.rename", map[string]any{"kind": "genre", "from": []string{"剧情"}, "to": "  "}, "bad_request")
	admin.expectCode("admin.tags.delete", map[string]any{"kind": "genre", "names": []string{}}, "bad_request")
	admin.must("admin.users.create", map[string]any{"username": "bob", "password": "bob-password"}, nil)
	bob := e.newClient()
	bob.must("auth.login", map[string]string{"username": "bob", "password": "bob-password"}, nil)
	bob.expectCode("admin.tags.list", map[string]any{"kind": "genre"}, "forbidden")
	bob.expectCode("admin.tags.rename", map[string]any{"kind": "genre", "from": []string{"剧情"}, "to": "x"}, "forbidden")
}
