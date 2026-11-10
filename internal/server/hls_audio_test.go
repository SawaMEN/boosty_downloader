package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHLSAudioMasterSelectsDefaultAndHighestVideo(t *testing.T) {
	base := "https://cdn.example/master.m3u8?signature=xyz"
	m := map[string][]byte{base: []byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="RU",DEFAULT=YES,URI="ru.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="EN",DEFAULT=NO,URI="en.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=200,AUDIO="audio"
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=700,AUDIO="audio"
high.m3u8
`), "https://cdn.example/high.m3u8?signature=xyz": []byte("#EXTM3U\n#EXTINF:1,\nv.ts\n#EXT-X-ENDLIST\n"), "https://cdn.example/ru.m3u8?signature=xyz": []byte("#EXTM3U\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST\n")}
	v, a, e := resolveHLSWithAudio(context.Background(), dashFixture(m), base)
	if e != nil || len(v) != 1 || len(a) != 1 || v[0].url != "https://cdn.example/v.ts?signature=xyz" || a[0].url != "https://cdn.example/a.ts?signature=xyz" {
		t.Fatalf("video %+v audio %+v error %v", v, a, e)
	}
}
func TestHLSAudioRejectUnlistedAndUnsafe(t *testing.T) {
	base := "https://cdn.example/master.m3u8"
	for _, line := range []string{`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",URI="http://evil/a.m3u8"`, `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="other",URI="a.m3u8"`} {
		data := fmt.Sprintf("#EXTM3U\n%s\n#EXT-X-STREAM-INF:BANDWIDTH=100,AUDIO=\"aud\"\nv.m3u8\n", line)
		fetch := dashFixture(map[string][]byte{base: []byte(data), "https://cdn.example/v.m3u8": []byte("#EXTM3U\n#EXTINF:1,\nv.ts\n#EXT-X-ENDLIST\n")})
		_, _, e := resolveHLSWithAudio(context.Background(), fetch, base)
		if e == nil {
			t.Fatalf("accepted %q", line)
		}
	}
	if _, e := hlsMasterAttributeList(`TYPE=AUDIO,GROUP-ID="a,b",NAME="x"`); e != nil {
		t.Fatal(e)
	}
	if _, e := hlsMasterAttributeList(`TYPE=AUDIO,GROUP-ID="broken`); e == nil {
		t.Fatal("unterminated quote")
	}
	if _, e := hlsMasterAttributeList(strings.Repeat("X", 100)); e == nil {
		t.Fatal("invalid attributes")
	}
}

func TestHLSAudioAndVideoOffline(t *testing.T) {
	ffmpeg, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Skip("ffmpeg missing")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "video.ts")
	audio := filepath.Join(dir, "audio.ts")
	for _, v := range [][]string{
		{"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5", "-t", "1", "-c:v", "mpeg2video", "-f", "mpegts", video},
		{"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "1", "-c:a", "aac", "-f", "mpegts", audio},
	} {
		args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, v...)
		if b, e := exec.Command(ffmpeg, args...).CombinedOutput(); e != nil {
			t.Skipf("fixture %v %s", e, b)
		}
	}
	vb, e := os.ReadFile(video)
	if e != nil {
		t.Fatal(e)
	}
	ab, e := os.ReadFile(audio)
	if e != nil {
		t.Fatal(e)
	}
	base := "https://cdn.example/master.m3u8?token=z"
	m := map[string][]byte{
		base:                                     []byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"au\",NAME=\"Russian\",DEFAULT=YES,URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=400000,AUDIO=\"au\"\nvideo.m3u8\n"),
		"https://cdn.example/video.m3u8?token=z": []byte("#EXTM3U\n#EXTINF:1,\nvideo.ts\n#EXT-X-ENDLIST\n"),
		"https://cdn.example/audio.m3u8?token=z": []byte("#EXTM3U\n#EXTINF:1,\naudio.ts\n#EXT-X-ENDLIST\n"),
		"https://cdn.example/video.ts?token=z":   vb,
		"https://cdn.example/audio.ts?token=z":   ab,
	}
	dest := filepath.Join(dir, "output.mp4")
	size, digest, err := archiveHLS(context.Background(), dashFixture(m), base, dest)
	if err != nil {
		t.Fatal(err)
	}
	if size < 100 || len(digest) != 64 {
		t.Fatalf("bad output %d %s", size, digest)
	}
	if ffprobe, e := exec.LookPath("ffprobe"); e == nil {
		b, e := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", dest).Output()
		if e != nil || !strings.Contains(string(b), "video") || !strings.Contains(string(b), "audio") {
			t.Fatalf("streams %q %v", b, e)
		}
	}
}
