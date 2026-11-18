package library

import (
	"testing"
	"time"
)

func TestWriting(t *testing.T) {
	old := FileSettleTime
	defer func() { FileSettleTime = old }()
	FileSettleTime = 2 * time.Minute
	now := time.Now()
	for _, c := range []struct {
		name  string
		mtime time.Time
		want  bool
	}{
		{"刚修改", now.Add(-10 * time.Second), true},
		{"稍早于等待时间", now.Add(-119 * time.Second), true},
		{"超过等待时间", now.Add(-3 * time.Minute), false},
		{"很久以前", now.Add(-24 * time.Hour), false},
		{"时钟略快", now.Add(30 * time.Second), true},
		{"时钟快很多", now.Add(time.Hour), false},
	} {
		if got := writing(c.mtime, now); got != c.want {
			t.Errorf("%s：writing=%v，期望 %v", c.name, got, c.want)
		}
	}
	FileSettleTime = 0
	if writing(now, now) {
		t.Error("等待时间为 0 时不应跳过任何文件")
	}
}
