package mteam

import "testing"

func TestCleanMTteamTitle(t *testing.T) {
	in := "斗罗大陆Ⅱ绝世唐门.年番1.Soul.Land.2.The.Peerless.Tang.Clan.S01.2023.2160p.WEB-DL.HEVC.DDP.2.0.4Audio-StarfallWeb"
	out := CleanMTteamTitle(in)
	want := "Soul Land 2 The Peerless Tang Clan S01 2023 2160p WEB-DL H.265 DDP.2.0 4Audio-StarfallWeb"
	if out.Name != want {
		t.Errorf("name = %q, want %q", out.Name, want)
	}
	if out.SmallDescrCN != "斗罗大陆Ⅱ绝世唐门.年番1" {
		t.Errorf("cn = %q, want %q", out.SmallDescrCN, "斗罗大陆Ⅱ绝世唐门.年番1")
	}
	if out.Group != "StarfallWeb" {
		t.Errorf("group = %q, want StarfallWeb", out.Group)
	}
}

func TestCleanMTteamTitle2(t *testing.T) {
	in := "KAMUI.Hes.Behind.You.S01E05.1080p.LINETV.WEB-DL.AAC2.0.H.264-MWeb"
	out := CleanMTteamTitle(in)
	want := "KAMUI Hes Behind You S01E05 1080p LINETV WEB-DL AAC2.0 H.264-MWeb"
	if out.Name != want {
		t.Errorf("name = %q, want %q", out.Name, want)
	}
	if out.Group != "MWeb" {
		t.Errorf("group = %q, want MWeb", out.Group)
	}
}

func TestSplitSmallDescr(t *testing.T) {
	name, cn := SplitSmallDescr("斗罗大陆Ⅱ绝世唐门.年番1.Soul.Land.2.S01.2023.2160p.WEB-DL.HEVC")
	if cn == "" {
		t.Errorf("cn should not be empty")
	}
	if name == "" {
		t.Errorf("name should not be empty")
	}
}
