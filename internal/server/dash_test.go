package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func dashFixture(m map[string][]byte) mediaFetcher {
	return func(_ context.Context, u, _, _ string) (*http.Response, error) {
		b, ok := m[u]
		if !ok {
			return nil, fmt.Errorf("unexpected URL %s", u)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b)), Header: make(http.Header)}, nil
	}
}
func TestDASHRejectUnsupported(t *testing.T) {
	base := "https://cdn.example/v/manifest.mpd?auth=secret"
	samples := []string{
		`<MPD type="dynamic" mediaPresentationDuration="PT2S"><Period/></MPD>`,
		`<MPD type="static" mediaPresentationDuration="PT2S"><Period><ContentProtection/></Period></MPD>`,
		`<MPD type="static" mediaPresentationDuration="PT2S"><Period/><Period/></MPD>`,
		`<MPD type="static" mediaPresentationDuration="PT2S"><Period><AdaptationSet mimeType="video/mp4"><Representation bandwidth="200" id="v"><SegmentTemplate media="x-$Number$.m4s" initialization="i.mp4" duration="1"/><ContentProtection/></Representation></AdaptationSet></Period></MPD>`,
		`<MPD type="static" mediaPresentationDuration="PT2S"><Period><AdaptationSet mimeType="video/mp4"><Representation bandwidth="200" id="v"><SegmentTemplate media="x-$Number$.m4s" initialization="i.mp4"><SegmentTimeline><S d="1" r="-1"/></SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period></MPD>`,
	}
	for _, raw := range samples {
		_, _, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(raw)}), base)
		if err == nil {
			t.Fatal("accepted", raw)
		}
	}
}
func TestDASHSegmentTemplateAndQueries(t *testing.T) {
	base := "https://cdn.example/main.mpd?token=mine"
	mpd := `<MPD type="static" mediaPresentationDuration="PT2S"><Period><AdaptationSet contentType="video" mimeType="video/mp4"><Representation id="low" bandwidth="100"><SegmentTemplate timescale="1" duration="1" initialization="init-$RepresentationID$.mp4" media="low-$Number$.m4s"/></Representation><Representation id="high" bandwidth="300"><SegmentTemplate timescale="1" duration="1" initialization="init-$RepresentationID$.mp4" media="high-$Number%05d$.m4s"/></Representation></AdaptationSet><AdaptationSet contentType="audio" mimeType="audio/mp4"><Representation id="a" bandwidth="64"><SegmentTemplate timescale="1000" initialization="init-$RepresentationID$.mp4" media="a-$Time$.m4s"><SegmentTimeline><S t="0" d="1000" r="1"/></SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period></MPD>`
	video, audio, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(mpd)}), base)
	if err != nil {
		t.Fatal(err)
	}
	if video.init != "https://cdn.example/init-high.mp4?token=mine" || len(video.segments) != 2 || video.segments[0] != "https://cdn.example/high-00001.m4s?token=mine" || video.segments[1] != "https://cdn.example/high-00002.m4s?token=mine" {
		t.Fatalf("video %+v", video)
	}
	if audio == nil || audio.segments[1] != "https://cdn.example/a-1000.m4s?token=mine" {
		t.Fatalf("audio %+v", audio)
	}
}
func TestDASHSegmentList(t *testing.T) {
	base := "https://cdn.example/manifest.mpd"
	mpd := `<MPD type="static" mediaPresentationDuration="PT1S"><Period><AdaptationSet mimeType="video/mp4"><Representation bandwidth="100" id="v"><SegmentList><Initialization sourceURL="init.mp4"/><SegmentURL media="seg1.m4s"/></SegmentList></Representation></AdaptationSet></Period></MPD>`
	v, a, e := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(mpd)}), base)
	if e != nil || a != nil || len(v.segments) != 1 {
		t.Fatalf("%+v %+v %v", v, a, e)
	}
}
func fragmented(t *testing.T, ffmpeg, dir, kind string) ([]byte, [][]byte) {
	t.Helper()
	output := filepath.Join(dir, kind+".mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if kind == "audio" {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "2", "-c:a", "aac")
	} else {
		args = append(args, "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5", "-t", "2", "-c:v", "mpeg4")
	}
	args = append(args, "-movflags", "empty_moov+frag_keyframe+default_base_moof", "-frag_duration", "1000000", output)
	if b, e := exec.Command(ffmpeg, args...).CombinedOutput(); e != nil {
		t.Skipf("ffmpeg fixture: %v %s", e, b)
	}
	data, e := os.ReadFile(output)
	if e != nil {
		t.Fatal(e)
	}
	indexes := [][]byte{}
	from := 0
	for {
		i := bytes.Index(data[from:], []byte("moof"))
		if i < 0 {
			break
		}
		i += from - 4
		if i <= from {
			break
		}
		indexes = append(indexes, data[from:i])
		from = i
	}
	indexes = append(indexes, data[from:])
	if len(indexes) < 2 {
		t.Fatalf("did not fragment %s", kind)
	}
	return indexes[0], indexes[1:]
}
func TestDASHArchiveRemuxOffline(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	videoInit, videoParts := fragmented(t, ffmpeg, dir, "video")
	audioInit, audioParts := fragmented(t, ffmpeg, dir, "audio")
	url := "https://cdn.example/movie.mpd?sig=x"
	mpd := fmt.Sprintf(`<MPD type="static" mediaPresentationDuration="PT2S"><Period><AdaptationSet contentType="video" mimeType="video/mp4"><Representation id="v" bandwidth="300"><SegmentList><Initialization sourceURL="v-init.mp4"/>%s</SegmentList></Representation></AdaptationSet><AdaptationSet contentType="audio" mimeType="audio/mp4"><Representation id="a" bandwidth="64"><SegmentList><Initialization sourceURL="a-init.mp4"/>%s</SegmentList></Representation></AdaptationSet></Period></MPD>`, func() string {
		var b strings.Builder
		for i := range videoParts {
			fmt.Fprintf(&b, `<SegmentURL media="v-%d.m4s"/>`, i)
		}
		return b.String()
	}(), func() string {
		var b strings.Builder
		for i := range audioParts {
			fmt.Fprintf(&b, `<SegmentURL media="a-%d.m4s"/>`, i)
		}
		return b.String()
	}())
	m := map[string][]byte{url: []byte(mpd), "https://cdn.example/v-init.mp4?sig=x": videoInit, "https://cdn.example/a-init.mp4?sig=x": audioInit}
	for i, p := range videoParts {
		m[fmt.Sprintf("https://cdn.example/v-%d.m4s?sig=x", i)] = p
	}
	for i, p := range audioParts {
		m[fmt.Sprintf("https://cdn.example/a-%d.m4s?sig=x", i)] = p
	}
	dest := filepath.Join(dir, "archive.mp4")
	size, digest, err := archiveDASH(context.Background(), dashFixture(m), url, dest)
	if err != nil {
		t.Fatal(err)
	}
	if size < 100 || len(digest) != 64 {
		t.Fatalf("bad archive %d %s", size, digest)
	}
	probe, err := exec.LookPath("ffprobe")
	if err == nil {
		info, e := exec.Command(probe, "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", dest).Output()
		if e != nil || !strings.Contains(string(info), "video") || !strings.Contains(string(info), "audio") {
			t.Fatalf("streams %s %v", info, e)
		}
	}
}
func TestDASHFailurePreservesPreviousArchive(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "video.mp4")
	if e := os.WriteFile(dest, []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	url := "https://cdn.example/movie.mpd"
	mpd := `<MPD type="static" mediaPresentationDuration="PT1S"><Period><AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="100"><SegmentList><Initialization sourceURL="init.mp4"/><SegmentURL media="missing.m4s"/></SegmentList></Representation></AdaptationSet></Period></MPD>`
	_, _, err := archiveDASH(context.Background(), dashFixture(map[string][]byte{url: []byte(mpd)}), url, dest)
	if err == nil {
		t.Fatal("missing segment accepted")
	}
	got, e := os.ReadFile(dest)
	if e != nil || string(got) != "original" {
		t.Fatalf("previous video changed: %s %v", got, e)
	}
}
