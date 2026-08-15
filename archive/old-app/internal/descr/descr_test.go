package descr

import (
	"strings"
	"testing"
)

func TestExtractSections(t *testing.T) {
	src := `<fieldset>
  <legend>影片信息</legend><br />
  <img class="attach" src="https://img.example.com/poster/tt6485574.jpg" border="0" alt="poster" /><br />
  <b> 片名: </b>Zhang Ga the Soldier Boy<br />
  <b> 中文片名: </b>小兵张嘎<br />
  <b> 年代: </b>1963<br />
  <b> 导演: </b>崔嵬 / 欧阳红樱<br />
  <b> 剧情概要:</b><br />
  抗日战争时期。<br />
  <br />
  <img class="attach" src="https://img.example.com/still/tt6485574_1.jpg" border="0" /><br />
  <b> IMDb链接： </b><a href="https://www.imdb.com/title/tt6485574/" target="_blank">tt6485574</a><br />
  <b> 豆瓣链接： </b><a href="https://movie.douban.com/subject/1437595/" target="_blank">豆瓣</a><br />
</fieldset>`
	sec := ExtractSections(src)
	if sec["title"] != "Zhang Ga the Soldier Boy" {
		t.Errorf("title = %v", sec["title"])
	}
	if sec["chinese_title"] != "小兵张嘎" {
		t.Errorf("chinese_title = %v", sec["chinese_title"])
	}
	if sec["year_num"] != "1963" {
		t.Errorf("year_num = %v", sec["year_num"])
	}
	if sec["director"] != "崔嵬 / 欧阳红樱" {
		t.Errorf("director = %v", sec["director"])
	}
	if sec["imdb"] != "tt6485574" {
		t.Errorf("imdb = %v", sec["imdb"])
	}
	if sec["douban_id"] != "1437595" {
		t.Errorf("douban_id = %v", sec["douban_id"])
	}
	if !strings.Contains(sec["plot"].(string), "抗日战争") {
		t.Errorf("plot = %v", sec["plot"])
	}
}

func TestNormalizeAndStrip(t *testing.T) {
	src := `  <img src="https://dev.internal-source.org/pic/logo.png" alt="logo" /><br />
<fieldset>
  <b> 片名: </b>Tales of Herding Gods S01E89<br />
  <b> 中文片名: </b>牧神记<br />
</fieldset>
<br />
转载自 YY 发布组 出品`
	out := StripSourceReferences(src)
	out = NormalizeDescription(out)
	if strings.Contains(out, "logo.png") {
		t.Errorf("logo not removed: %s", out)
	}
	if strings.Contains(out, "转载自") {
		t.Errorf("reference not removed: %s", out)
	}
	if !strings.Contains(out, "牧神记") {
		t.Errorf("body not preserved: %s", out)
	}
}

func TestDescrBuilder(t *testing.T) {
	files := []map[string]any{
		{"path": "Zhang.Ga.the.Soldier.Boy.1963.1440p.mkv", "size": int64(7100000000)},
		{"path": "Sample/README.txt", "size": int64(1234)},
	}
	parsed := map[string]any{
		"name":       "Zhang Ga the Soldier Boy 1963",
		"files":      files,
		"file_count": 2,
		"total_size": int64(7100001234),
	}
	itemFields := map[string]any{
		"title":        "Zhang Ga the Soldier Boy 1963 1440p WEB-DL H.265 DDP 2.0 2Audio-LongWeb",
		"description":  `<fieldset><b>片名:</b>Zhang Ga the Soldier Boy<br /><b>年代:</b>1963</fieldset>转自 某某高清 发布`,
		"small_descr":  "小兵张嘎 1963 1440p",
		"imdb":         "tt6485574",
	}
	b := NewDescrBuilder()
	descrHTML := b.Build(itemFields, parsed)
	if !strings.Contains(descrHTML, "<table") || !strings.Contains(descrHTML, "文件列表") {
		t.Errorf("missing file list table: %s", descrHTML)
	}
	if strings.Contains(descrHTML, "转自") {
		t.Errorf("reference not stripped: %s", descrHTML)
	}
	if !strings.Contains(descrHTML, "tt6485574") {
		t.Errorf("imdb not in descr: %s", descrHTML)
	}
	if !strings.Contains(descrHTML, "小兵张嘎") {
		t.Errorf("small_descr not attached: %s", descrHTML)
	}
}

func TestDescrBuilderFileListText(t *testing.T) {
	b := NewDescrBuilder()
	out := b.Build(map[string]any{
		"title": "Have a Feast 2024 S01 Complete 2160p",
		"descr": `<fieldset><b>片名:</b>Have a Feast</fieldset><br />来自 dev.internal-source.org 发布`,
	}, map[string]any{
		"file_list_text": "Have.a.Feast.2024.S01.2160p.mkv  22921729683",
	})
	if !strings.Contains(out, "<pre>") || !strings.Contains(out, "文件列表") {
		t.Errorf("file_list_text fallback missing: %s", out)
	}
	if strings.Contains(out, "来自 dev.internal-source.org") {
		t.Errorf("reference not stripped: %s", out)
	}
}

func TestHumanSize(t *testing.T) {
	if got := humanSize(int64(7100000000)); got != "6.61 GiB" {
		t.Errorf("humanSize = %q, want %q", got, "6.61 GiB")
	}
	if got := humanSize(1024); got != "1.00 KiB" {
		t.Errorf("humanSize = %q", got)
	}
}
