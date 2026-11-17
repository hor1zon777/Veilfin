//go:build ffmpeg_embed

package media

import _ "embed"

// ffbin/ffmpeg.tar.gz 由 scripts/build-ffmpeg.sh 生成（不提交到仓库），见 ffbin/README.md。
//
//go:embed ffbin/ffmpeg.tar.gz
var embeddedBundle []byte

func init() { bundled = embeddedBundle }
