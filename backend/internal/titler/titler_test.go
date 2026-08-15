package titler

import (
	"strings"
	"testing"
)

func ptr(s string) *string { return &s }

func TestParseTitle(t *testing.T) {
	c := ParseTitle("Zhang Ga the Soldier Boy 1963 1440p WEB-DL H.265 DDP 2.0 2Audio-LongWeb")
	if c.Title != "Zhang Ga the Soldier Boy" {
		t.Errorf("title = %q, want %q", c.Title, "Zhang Ga the Soldier Boy")
	}
	if c.Year == nil || *c.Year != "1963" {
		t.Errorf("year = %v, want 1963", c.Year)
	}
	if c.Resolution == nil || *c.Resolution != "1440p" {
		t.Errorf("resolution = %v, want 1440p", c.Resolution)
	}
	if c.VideoCodec == nil || *c.VideoCodec != "HEVC" {
		t.Errorf("video_codec = %v, want HEVC", c.VideoCodec)
	}
	if c.AudioCodec == nil || *c.AudioCodec != "DDP" {
		t.Errorf("audio_codec = %v, want DDP", c.AudioCodec)
	}
	if c.Source == nil || *c.Source != "WEB-DL" {
		t.Errorf("source = %v, want WEB-DL", c.Source)
	}
	if c.Medium == nil || *c.Medium != "WEB" {
		t.Errorf("medium = %v, want WEB", c.Medium)
	}
	if c.Channels == nil || *c.Channels != "2.0 2Audio" {
		t.Errorf("channels = %v, want %q", c.Channels, "2.0 2Audio")
	}
	if c.Group == nil || *c.Group != "LongWeb" {
		t.Errorf("group = %v, want LongWeb", c.Group)
	}
	if c.Complete {
		t.Errorf("complete should be false")
	}
}

func TestParseTitleComplete(t *testing.T) {
	c := ParseTitle("Have a Feast 2024 S01 Complete 2160p WEB-DL H.265 DDP 2.0-LongWeb")
	if c.Title != "Have a Feast" {
		t.Errorf("title = %q, want %q", c.Title, "Have a Feast")
	}
	if c.Season == nil || *c.Season != "1" {
		t.Errorf("season = %v, want 1", c.Season)
	}
	if !c.Complete {
		t.Errorf("complete should be true")
	}
	if c.Resolution == nil || *c.Resolution != "2160p" {
		t.Errorf("resolution = %v, want 2160p", c.Resolution)
	}
}

func TestStandardKeys(t *testing.T) {
	c := ParseTitle("Zhang Ga the Soldier Boy 1963 1440p WEB-DL H.265 DDP 2.0 2Audio-LongWeb")
	keys := StandardKeys(c)
	expect := map[string]*string{
		"category":    ptr("movie"),
		"resolution":  ptr("1440"),
		"source":      ptr("WEB-DL"),
		"medium":      ptr("WEB"),
		"video_codec": ptr("HEVC"),
		"audio_codec": ptr("DDP"),
		"channels":    ptr("2.0 2AUDIO"),
	}
	for k, want := range expect {
		got, ok := keys[k]
		if !ok || (got == nil) != (want == nil) {
			t.Errorf("key %s: got %v (present=%v), want %v", k, got, ok, want)
			continue
		}
		if want != nil && *got != *want {
			t.Errorf("key %s: got %v, want %v", k, *got, *want)
		}
	}
	if keys["hdr"] != nil {
		t.Errorf("hdr = %v, want nil", keys["hdr"])
	}
}

func TestParseTitleGroupNotEaten(t *testing.T) {
	c := ParseTitle("Good Will Hunting 1997 1080p BluRay DTS-HD MA 5.1 x264-AMIABLE")
	if c.Title != "Good Will Hunting" {
		t.Errorf("title = %q, want %q", c.Title, "Good Will Hunting")
	}
	if c.Group == nil || *c.Group != "AMIABLE" {
		t.Errorf("group = %v, want AMIABLE", c.Group)
	}
	if c.VideoCodec == nil || *c.VideoCodec != "H264" {
		t.Errorf("video_codec = %v, want H264", c.VideoCodec)
	}
	if c.AudioCodec == nil || *c.AudioCodec != "DTS-HD MA" {
		t.Errorf("audio_codec = %v, want DTS-HD MA", c.AudioCodec)
	}
	if c.Channels == nil || *c.Channels != "5.1" {
		t.Errorf("channels = %v, want 5.1", c.Channels)
	}
}

// TestParseTitleCodecNotSwallowed 修正历史 bug:`AAC2.0.H.264-MWeb` 的制作组
// 不得吞掉 AAC / H.264 / 2.0(旧版会把 group 解析成 `DLAAC20H264MWeb`)。
func TestParseTitleCodecNotSwallowed(t *testing.T) {
	c := ParseTitle("KAMUI.Hes.Behind.You.S01E05.1080p.LINETV.WEB-DL.AAC2.0.H.264-MWeb")
	if c.Group == nil || *c.Group != "MWeb" {
		t.Errorf("group = %v, want MWeb", c.Group)
	}
	if c.VideoCodec == nil || *c.VideoCodec != "H264" {
		t.Errorf("video_codec = %v, want H264 (H.264 must not be swallowed)", c.VideoCodec)
	}
	if c.Source == nil || *c.Source != "WEB-DL" {
		t.Errorf("source = %v, want WEB-DL", c.Source)
	}
	if c.Season == nil || *c.Season != "1" {
		t.Errorf("season = %v, want 1", c.Season)
	}
	if c.Episode == nil || *c.Episode != "5" {
		t.Errorf("episode = %v, want 5", c.Episode)
	}
	// `AAC2.0` 是紧凑粘连写法(旧组件规则按词边界切分,无法拆成 AAC + 2.0),
	// 但它必须留在残文里,而不是被制作组吞掉。
	if !strings.Contains(c.Title, "AAC2.0") {
		t.Errorf("title = %q, want it to retain AAC2.0 (not swallowed)", c.Title)
	}
}

// TestParseTitleDTSHDNotSwallowed 修正历史 bug:`DTS-HD.MA.5.1-THOR` 的制作组
// 不得吞掉 MA / 5.1。
func TestParseTitleDTSHDNotSwallowed(t *testing.T) {
	c := ParseTitle("The.Expanse.S01E01-S01E02.2015.1080p.BluRay.x264.DTS-HD.MA.5.1-THOR")
	if c.Group == nil || *c.Group != "THOR" {
		t.Errorf("group = %v, want THOR", c.Group)
	}
	if c.VideoCodec == nil || *c.VideoCodec != "H264" {
		t.Errorf("video_codec = %v, want H264", c.VideoCodec)
	}
	if c.AudioCodec == nil || *c.AudioCodec != "DTS-HD" {
		t.Errorf("audio_codec = %v, want DTS-HD", c.AudioCodec)
	}
	if c.Channels == nil || *c.Channels != "5.1" {
		t.Errorf("channels = %v, want 5.1", c.Channels)
	}
}
