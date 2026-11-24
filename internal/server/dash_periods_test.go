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

func TestDASHMultiPeriodPlan(t *testing.T) {
	const base = "https://cdn.example/main.mpd?token=private"
	period := func(start, duration, folder string) string {
		return fmt.Sprintf(`<Period %s %s><BaseURL>%s/</BaseURL><AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="100"><SegmentTemplate initialization="init.mp4" media="s-$Number$.m4s" duration="1"/></Representation></AdaptationSet></Period>`, start, duration, folder)
	}
	first := period("", "", "one")
	second := period(`start="PT2S"`, "", "two")
	wrap := func(p string) string {
		return `<MPD type="static" mediaPresentationDuration="PT5S">` + p + "</MPD>"
	}
	raw := wrap(first + second)
	plans, err := resolveDASHPresentation(context.Background(), dashFixture(map[string][]byte{base: []byte(raw)}), base)
	if err != nil || len(plans) != 2 || len(plans[0].video.segments) != 2 || len(plans[1].video.segments) != 3 {
		t.Fatalf("invalid period plan: %+v %v", plans, err)
	}
	if plans[1].video.init != "https://cdn.example/two/init.mp4?token=private" || plans[0].duration != 2 || plans[1].duration != 3 {
		t.Fatalf("incorrect period boundary or base URL: %+v", plans)
	}
	for _, bad := range []string{
		wrap(period("", `duration="PT1S"`, "one") + second),
		wrap(period("", `duration="PT3S"`, "one") + second),
		wrap(first + period("", "", "two")),
		wrap(first + period(`start="PT2S"`, `duration="PT4S"`, "two")),
		strings.Replace(raw, "<BaseURL>two/", "<ContentProtection/><BaseURL>two/", 1),
		strings.Replace(raw, `type="static"`, `type="dynamic"`, 1),
	} {
		if _, err := resolveDASHPresentation(context.Background(), dashFixture(map[string][]byte{base: []byte(bad)}), base); err == nil {
			t.Fatalf("unsafe multi-period presentation accepted: %s", bad)
		}
	}
}

func TestDASHMultiPeriodRemuxOffline(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	init, parts := fragmented(t, ffmpeg, dir, "video")
	const base = "https://cdn.example/main.mpd"
	assets := map[string][]byte{}
	var manifest strings.Builder
	manifest.WriteString(`<MPD type="static" mediaPresentationDuration="PT4S">`)
	for i := 0; i < 2; i++ {
		prefix := fmt.Sprintf("p%d", i)
		fmt.Fprintf(&manifest, `<Period start="PT%dS" duration="PT2S"><BaseURL>%s/</BaseURL><AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="100"><SegmentList><Initialization sourceURL="init.mp4"/>`, i*2, prefix)
		assets["https://cdn.example/"+prefix+"/init.mp4"] = init
		for j, part := range parts {
			name := fmt.Sprintf("s%d.m4s", j)
			fmt.Fprintf(&manifest, `<SegmentURL media="%s"/>`, name)
			assets["https://cdn.example/"+prefix+"/"+name] = part
		}
		manifest.WriteString("</SegmentList></Representation></AdaptationSet></Period>")
	}
	manifest.WriteString("</MPD>")
	assets[base] = []byte(manifest.String())
	dest := filepath.Join(dir, "joined.mp4")
	if size, digest, err := archiveDASH(context.Background(), dashFixture(assets), base, dest); err != nil || size <= 0 || len(digest) != 64 {
		t.Fatalf("multi-period archive failed: size=%d err=%v", size, err)
	}
	old, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	delete(assets, "https://cdn.example/p1/init.mp4")
	if _, _, err := archiveDASH(context.Background(), dashFixture(assets), base, dest); err == nil {
		t.Fatal("missing second period accepted")
	}
	current, err := os.ReadFile(dest)
	if err != nil || string(current) != string(old) {
		t.Fatal("failed second period replaced previous archive")
	}
}
