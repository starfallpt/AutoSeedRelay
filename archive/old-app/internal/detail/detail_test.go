package detail

import "testing"

const sampleFileListHTML = `<html><body>
<tr><td class=rowfollow>Test.Movie.2026.mkv</td><td class=rowfollow align="right">6.70 GB</td></tr>
<tr><td class=rowfollow>Sample/README.txt</td><td class=rowfollow align="right">918.08 MB</td></tr>
</body></html>`

const sampleDetailsHTML = `<html><body>
<table>
<tr><td class="rowhead">副标题</td><td class="rowfollow">测试副标题 1080p</td></tr>
<tr><td class="rowhead">标签</td><td class="rowfollow"><span style="background:yellow">国语</span><span style="background:red">中字</span></td></tr>
<tr><td class="rowhead">IMDb</td><td class="rowfollow"><a href="https://www.imdb.com/title/tt6485574/">tt6485574</a></td></tr>
</table>
</body></html>`

func TestParseHumanSize(t *testing.T) {
	if got := ParseHumanSize("6.70 GB"); got == nil || *got != 7194070220 {
		t.Errorf("6.70 GB = %v, want 7194070220", got)
	}
	if got := ParseHumanSize("918.08 MB"); got == nil || *got != 962676654 {
		t.Errorf("918.08 MB = %v, want 962676654", got)
	}
	if ParseHumanSize("abc") != nil {
		t.Errorf("invalid size should be nil")
	}
}

func TestParseFileListHTML(t *testing.T) {
	files := ParseFileListHTML(sampleFileListHTML)
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	f0 := files[0]
	if f0.Name != "Test.Movie.2026.mkv" || f0.Path != "Test.Movie.2026.mkv" {
		t.Errorf("f0 = %+v", f0)
	}
	if f0.Size == nil || *f0.Size != 7194070220 {
		t.Errorf("f0 size = %v, want 7194070220", f0.Size)
	}
	if f0.SizeHuman != "6.70 GB" {
		t.Errorf("f0 size_human = %q", f0.SizeHuman)
	}
}

func TestParseSmallDescr(t *testing.T) {
	if got := ParseSmallDescr(sampleDetailsHTML); got != "测试副标题 1080p" {
		t.Errorf("small_descr = %q", got)
	}
}

func TestParseTags(t *testing.T) {
	tags := ParseTags(sampleDetailsHTML)
	if len(tags) != 2 || tags[0] != "国语" || tags[1] != "中字" {
		t.Errorf("tags = %v", tags)
	}
}

func TestParseIMDB(t *testing.T) {
	if got := ParseIMDB(sampleDetailsHTML); got != "tt6485574" {
		t.Errorf("imdb = %q", got)
	}
}

func TestParseCookie(t *testing.T) {
	c := ParseCookie("access_token=abc; csrftoken=xyz")
	if c["access_token"] != "abc" || c["csrftoken"] != "xyz" {
		t.Errorf("cookie = %v", c)
	}
}
