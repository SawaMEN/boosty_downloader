package server

import (
	"context"
	"strings"
	"testing"
)

func TestDASHTemplateInheritance(t *testing.T) {
	const base = "https://cdn.example/main.mpd?token=secret"
	mpd := `<MPD type="static" mediaPresentationDuration="PT4S"><Period start="PT0S" duration="PT4S" BaseURL=""><SegmentTemplate timescale="2" initialization="init-$RepresentationID$.mp4" media="default-$Number$.m4s" duration="2" startNumber="7"/><AdaptationSet mimeType="video/mp4"><SegmentTemplate media="video-$Number%03d$.m4s"/><Representation id="low" bandwidth="100"><SegmentTemplate startNumber="2"/></Representation><Representation id="high" bandwidth="200"><SegmentTemplate startNumber="0" duration="4"/></Representation></AdaptationSet><AdaptationSet mimeType="audio/mp4"><Representation id="a" bandwidth="64"><SegmentTemplate media="audio-$Number$.m4s"/></Representation></AdaptationSet></Period></MPD>`
	video, audio, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(mpd)}), base)
	if err != nil || audio == nil {
		t.Fatalf("inherited template failed: %v", err)
	}
	if video.init != "https://cdn.example/init-high.mp4?token=secret" || len(video.segments) != 2 ||
		video.segments[0] != "https://cdn.example/video-000.m4s?token=secret" ||
		video.segments[1] != "https://cdn.example/video-001.m4s?token=secret" {
		t.Fatalf("incorrect video inheritance: %+v", video)
	}
	if audio.init != "https://cdn.example/init-a.mp4?token=secret" || len(audio.segments) != 4 ||
		audio.segments[0] != "https://cdn.example/audio-7.m4s?token=secret" {
		t.Fatalf("video override leaked into audio: %+v", audio)
	}
}

func TestDASHInheritedTimelineReplacement(t *testing.T) {
	const base = "https://cdn.example/main.mpd"
	mpd := `<MPD type="static" mediaPresentationDuration="PT4S"><Period><SegmentTemplate timescale="1" initialization="init.mp4" media="s-$Time$.m4s"><SegmentTimeline><S t="0" d="1" r="3"/></SegmentTimeline></SegmentTemplate><AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="100"><SegmentTemplate><SegmentTimeline><S t="0" d="2" r="1"/></SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period></MPD>`
	video, _, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(mpd)}), base)
	if err != nil || len(video.segments) != 2 || video.segments[1] != "https://cdn.example/s-2.m4s" {
		t.Fatalf("child timeline did not replace parent: %+v %v", video, err)
	}
	for _, bad := range []string{
		strings.Replace(mpd, `timescale="1"`, `timescale="0"`, 1),
		strings.Replace(mpd, `timescale="1"`, `timescale="1" presentationTimeOffset="1"`, 1),
		strings.Replace(mpd, "<Period>", `<Period start="PT1S">`, 1),
		strings.Replace(mpd, `<S t="0" d="2" r="1"/>`, "", 1),
		strings.Replace(mpd, `<Representation id="v" bandwidth="100">`, `<Representation id="v" bandwidth="100"><SegmentList/>`, 1),
	} {
		if _, _, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(bad)}), base); err == nil {
			t.Fatalf("unsafe inherited timeline accepted: %s", bad)
		}
	}
}

func TestDASHTemplateLiteralDollar(t *testing.T) {
	got, err := expandDASHTemplate("cash-$$-$Number%03d$-$Time$.m4s", "v", 100, 2, 7)
	if err != nil || got != "cash-$-002-7.m4s" {
		t.Fatalf("escaped dollar: %q %v", got, err)
	}
	for _, bad := range []string{"$Unknown$", "missing-$", "$Number%099d$", "$$-$Other$"} {
		if _, err := expandDASHTemplate(bad, "v", 100, 2, 7); err == nil {
			t.Fatalf("accepted malformed template %q", bad)
		}
	}
}
