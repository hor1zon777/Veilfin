package db

import (
	"context"
	"path/filepath"
	"testing"
)

// v4 迁移：进度条预览图从全局开关改为按媒体库开关，已有媒体库沿用原来的全局设置。
func TestMigrateLibraryTrickplay(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		setting string // 升级前保存的全局设置，空表示从没保存过
		want    bool
	}{
		{"全局关闭", `{"serverName":"x","trickplayEnabled":false}`, false},
		{"全局开启", `{"serverName":"x","trickplayEnabled":true}`, true},
		{"没有保存过设置", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.db")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			id, err := d.CreateLibrary(ctx, &Library{Name: "库", Kind: "movie", Scraper: "none", Paths: []string{"/x"}, Trickplay: true})
			if err != nil {
				t.Fatal(err)
			}
			if c.setting != "" {
				if err := d.SetSetting(ctx, "config", c.setting); err != nil {
					t.Fatal(err)
				}
			}
			// 退回到 v3 的表结构，模拟从旧版本升级
			for _, q := range []string{"ALTER TABLE users DROP COLUMN allow_transcode", "ALTER TABLE libraries DROP COLUMN trickplay", "PRAGMA user_version = 3"} {
				if _, err := d.W.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			d.Close()

			d, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			l, err := d.LibraryByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if l.Trickplay != c.want {
				t.Fatalf("升级后 trickplay=%v，期望 %v", l.Trickplay, c.want)
			}
			// 之后可以按媒体库修改
			l.Trickplay = !c.want
			if err := d.UpdateLibrary(ctx, l); err != nil {
				t.Fatal(err)
			}
			if l2, _ := d.LibraryByID(ctx, id); l2.Trickplay != !c.want {
				t.Fatal("修改媒体库的预览图开关没有保存")
			}
		})
	}
}
