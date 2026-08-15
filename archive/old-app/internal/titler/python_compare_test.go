package titler

import "testing"

// TestAgainstPythonGroundTruth 复现 Python parse_title 的行为(含其怪癖)。
func TestAgainstPythonGroundTruth(t *testing.T) {
	cases := []struct {
		raw       string
		title     string
		season    string
		episode   string
		year      string
		res       string
		src       string
		med       string
		vc        string
		ac        string
		ch        string
		grp       string
		complete  bool
	}{
		{
			raw: "KAMUI.Hes.Behind.You.S01E05.1080p.LINETV.WEB-DL.AAC2.0.H.264-MWeb",
			title: "KAMUI.Hes.Behind.You. . .LINETV", season: "1", episode: "5",
			res: "1080p", grp: "DLAAC20H264MWeb",
		},
		{
			raw: "牧神记.Tales.of.Herding.Gods.S01E89.2024.1080p.WEB-DL.HEVC.AAC2.0",
			title: "牧神记.Tales.of.Herding.Gods", season: "1", episode: "89",
			year: "2024", res: "1080p", grp: "AAC20",
		},
		{
			raw: "Muttertag.1993.1080p.BluRay.DD+5.1.x264-PLAN9",
			title: "Muttertag. . . .DD+", year: "1993", res: "1080p",
			src: "BluRay", med: "BLURAY", ch: "5.1", grp: "PLAN9",
		},
		{
			raw: "Zhang Ga the Soldier Boy 1963 1440p WEB-DL H.265 DDP 2.0 2Audio-LongWeb",
			title: "Zhang Ga the Soldier Boy", year: "1963", res: "1440p",
			src: "WEB-DL", med: "WEB", vc: "HEVC", ac: "DDP", ch: "2.0 2Audio", grp: "LongWeb",
		},
		{
			raw: "Have a Feast 2024 S01 Complete 2160p WEB-DL H.265 DDP 2.0-LongWeb",
			title: "Have a Feast", season: "1", year: "2024", res: "2160p",
			src: "WEB-DL", med: "WEB", vc: "HEVC", ac: "DDP", ch: "2.0", grp: "LongWeb", complete: true,
		},
		{
			raw: "Good Will Hunting 1997 1080p BluRay DTS-HD MA 5.1 x264-AMIABLE",
			title: "Good Will Hunting", year: "1997", res: "1080p",
			src: "BluRay", med: "BLURAY", vc: "H264", ac: "DTS-HD MA", ch: "5.1", grp: "AMIABLE",
		},
		{
			raw: "Soul.Land.2.S01.2023.2160p.WEB-DL.HEVC.DDP.2.0.4Audio-StarfallWeb",
			title: "Soul.Land.2. . . . . . . .4Audio", season: "1",
			year: "2023", res: "2160p", src: "WEB-DL", med: "WEB", vc: "HEVC", ac: "DDP", ch: "2.0", grp: "StarfallWeb",
		},
		{
			raw: "The.Expanse.S01E01-S01E02.2015.1080p.BluRay.x264.DTS-HD.MA.5.1-THOR",
			title: "The.Expanse", season: "1", episode: "1-2", year: "2015", res: "1080p",
			src: "BluRay", med: "BLURAY", grp: "HDMA51THOR",
		},
	}
	for _, c := range cases {
		got := ParseTitle(c.raw)
		eq := func(name, want string, gotPtr *string) bool {
			if want == "" {
				return gotPtr == nil
			}
			return gotPtr != nil && *gotPtr == want
		}
		if got.Title != c.title {
			t.Errorf("[%s] title = %q, want %q", c.raw, got.Title, c.title)
		}
		if !eq("season", c.season, got.Season) {
			t.Errorf("[%s] season = %v, want %q", c.raw, got.Season, c.season)
		}
		if !eq("episode", c.episode, got.Episode) {
			t.Errorf("[%s] episode = %v, want %q", c.raw, got.Episode, c.episode)
		}
		if !eq("year", c.year, got.Year) {
			t.Errorf("[%s] year = %v, want %q", c.raw, got.Year, c.year)
		}
		if !eq("res", c.res, got.Resolution) {
			t.Errorf("[%s] resolution = %v, want %q", c.raw, got.Resolution, c.res)
		}
		if !eq("src", c.src, got.Source) {
			t.Errorf("[%s] source = %v, want %q", c.raw, got.Source, c.src)
		}
		if !eq("med", c.med, got.Medium) {
			t.Errorf("[%s] medium = %v, want %q", c.raw, got.Medium, c.med)
		}
		if !eq("vc", c.vc, got.VideoCodec) {
			t.Errorf("[%s] video_codec = %v, want %q", c.raw, got.VideoCodec, c.vc)
		}
		if !eq("ac", c.ac, got.AudioCodec) {
			t.Errorf("[%s] audio_codec = %v, want %q", c.raw, got.AudioCodec, c.ac)
		}
		if !eq("ch", c.ch, got.Channels) {
			t.Errorf("[%s] channels = %v, want %q", c.raw, got.Channels, c.ch)
		}
		if !eq("grp", c.grp, got.Group) {
			t.Errorf("[%s] group = %v, want %q", c.raw, got.Group, c.grp)
		}
		if got.Complete != c.complete {
			t.Errorf("[%s] complete = %v, want %v", c.raw, got.Complete, c.complete)
		}
	}
}
