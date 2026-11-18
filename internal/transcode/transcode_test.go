package transcode

import (
	"reflect"
	"testing"
)

func TestProfilesFor(t *testing.T) {
	ids := func(ps []Profile) []string {
		out := []string{}
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}
	cases := []struct {
		w, h int
		want []string
	}{
		{3840, 2160, []string{"1080p", "720p", "480p", "360p"}}, // 4K：全部比原片小
		{1920, 1080, []string{"1080p", "720p", "480p", "360p"}}, // 同分辨率的 1080P（降码率 / 转换编码）+ 更低的
		{1280, 720, []string{"720p", "480p", "360p"}},
		{1080, 1920, []string{"1080p", "720p", "480p", "360p"}}, // 竖屏按短边
		{960, 540, []string{"720p", "480p", "360p"}},            // 不放大：720P 档实际输出 540
		{320, 180, []string{"360p"}},
		{0, 0, []string{}},
	}
	for _, c := range cases {
		if got := ids(ProfilesFor(c.w, c.h)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%dx%d: %v，期望 %v", c.w, c.h, got, c.want)
		}
	}
}
