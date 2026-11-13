package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// DASH-IF allows S@r=-1 in finite SegmentTimelines. Expansion must stop at
// the next explicit S@t, or at the final period boundary; it must never
// generate unbounded requests or change the VOD-only/DRM restrictions.
func dashNegativeRepeatMPD(duration, start, timescale, timeline string) string {
	return fmt.Sprintf(`<MPD type="static" mediaPresentationDuration="%s"><Period><AdaptationSet contentType="video" mimeType="video/mp4"><Representation id="v" bandwidth="100"><SegmentTemplate startNumber="%s" timescale="%s" initialization="init.mp4" media="clip-$Number$-$Time$.m4s"><SegmentTimeline>%s</SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period></MPD>`, duration, start, timescale, timeline)
}

func TestDASHFiniteNegativeTimelineRepeat(t *testing.T) {
	const base = "https://cdn.example/video/main.mpd?token=private"
	cases := []struct {
		name, duration, start, timescale, timeline string
		want                                       []string
	}{
		{
			name: "repeat to period end", duration: "PT5S", start: "1", timescale: "1000",
			timeline: `<S t="0" d="2000" r="-1"/>`,
			want:     []string{"clip-1-0.m4s", "clip-2-2000.m4s", "clip-3-4000.m4s"},
		},
		{
			name: "repeat to next explicit S time", duration: "PT5S", start: "5", timescale: "1000",
			timeline: `<S t="0" d="1000" r="-1"/><S t="3000" d="2000"/>`,
			want:     []string{"clip-5-0.m4s", "clip-6-1000.m4s", "clip-7-2000.m4s", "clip-8-3000.m4s"},
		},
		{
			name: "fractional duration last segment", duration: "PT2.5S", start: "1", timescale: "1000",
			timeline: `<S t="0" d="1000" r="-1"/>`,
			want:     []string{"clip-1-0.m4s", "clip-2-1000.m4s", "clip-3-2000.m4s"},
		},
		{
			name: "nonzero first presentation tick", duration: "PT12S", start: "1", timescale: "1000",
			timeline: `<S t="9000" d="1000" r="-1"/>`,
			want:     []string{"clip-1-9000.m4s", "clip-2-10000.m4s", "clip-3-11000.m4s"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := dashNegativeRepeatMPD(tc.duration, tc.start, tc.timescale, tc.timeline)
			video, audio, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(raw)}), base)
			if err != nil || audio != nil || len(video.segments) != len(tc.want) {
				t.Fatalf("timeline was not expanded to finite VOD: %+v %v", video, err)
			}
			for i, suffix := range tc.want {
				if video.segments[i] != "https://cdn.example/video/"+suffix+"?token=private" {
					t.Fatalf("segment %d: got %s, want %s", i, video.segments[i], suffix)
				}
			}
		})
	}
}

func TestDASHNegativeTimelineRepeatRejectsUnsafeOrUnbounded(t *testing.T) {
	const base = "https://cdn.example/main.mpd"
	cases := []struct {
		name, duration, start, timescale, timeline string
	}{
		{"unknown period bound", "PT20000S", "1", "1", `<S t="0" d="1" r="-1"/>`},
		{"invalid negative repeat", "PT4S", "1", "1", `<S t="0" d="1" r="-2"/>`},
		{"missing next timestamp", "PT4S", "1", "1", `<S t="0" d="1" r="-1"/><S d="1"/>`},
		{"backwards next timestamp", "PT4S", "1", "1", `<S t="2" d="1" r="-1"/><S t="1" d="1"/>`},
		{"crossed explicit boundary", "PT4S", "1", "1", `<S t="0" d="3" r="-1"/><S t="4" d="1"/>`},
		{"zero segment duration", "PT4S", "1", "1", `<S t="0" d="0" r="-1"/>`},
		{"period timescale overflow", "PT2S", "1", "18446744073709551615", `<S t="0" d="1" r="-1"/>`},
		{"tick counter overflow", "PT2S", "1", "1", `<S t="18446744073709551615" d="2"/>`},
		{"segment number overflow", "PT4S", "18446744073709551615", "1", `<S t="0" d="1" r="1"/>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := dashNegativeRepeatMPD(tc.duration, tc.start, tc.timescale, tc.timeline)
			if _, _, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(raw)}), base); err == nil {
				t.Fatal("unsafe or unbounded DASH SegmentTimeline accepted")
			}
		})
	}
}

func TestDASHNegativeRepeatStillRejectsDynamicAndDRM(t *testing.T) {
	base := "https://cdn.example/main.mpd"
	playlist := dashNegativeRepeatMPD("PT4S", "1", "1", `<S t="0" d="1" r="-1"/>`)
	for _, tampered := range []string{
		strings.Replace(playlist, `type="static"`, `type="dynamic"`, 1),
		strings.Replace(playlist, "<Representation", "<ContentProtection/><Representation", 1),
	} {
		if _, _, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(tampered)}), base); err == nil {
			t.Fatal("negative repeat support unexpectedly bypassed VOD-only/DRM checks")
		}
	}
}

func TestDASHDurationTemplateRejectsSegmentNumberWrap(t *testing.T) {
	const base = "https://cdn.example/main.mpd"
	raw := `<MPD type="static" mediaPresentationDuration="PT2S"><Period><AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="100"><SegmentTemplate startNumber="18446744073709551615" timescale="1" duration="1" initialization="init.mp4" media="part-$Number$.m4s"/></Representation></AdaptationSet></Period></MPD>`
	if _, _, err := resolveDASH(context.Background(), dashFixture(map[string][]byte{base: []byte(raw)}), base); err == nil {
		t.Fatal("SegmentTemplate duration path wrapped the last segment number")
	}
}
