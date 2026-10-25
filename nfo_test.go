package main

import (
	"strings"
	"testing"
)

func TestKodiNFOHeaderAndArtwork(t *testing.T) {
	season, episode := 1, 3
	text, err := nfoXML("episodedetails", &Metadata{
		Title:     "第三集 & 片名",
		Plot:      "分集简介",
		Directors: []string{"导演"},
		Genres:    []string{"儿童"},
		Tags:      []string{"自然拼读"},
	}, "episode-id", &season, &episode, "系列名", "03-thumb.jpg", "03-fanart.jpg")
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	if !strings.HasPrefix(text, wantHeader) {
		t.Fatalf("unexpected XML header: %q", text)
	}
	for _, want := range []string{
		"<episodedetails>",
		"<title>第三集 &amp; 片名</title>",
		"<showtitle>系列名</showtitle>",
		"<season>1</season>",
		"<episode>3</episode>",
		`<thumb aspect="thumb">03-thumb.jpg</thumb>`,
		"<fanart>\n    <thumb>03-fanart.jpg</thumb>\n  </fanart>",
		"<director>导演</director>",
		"<uniqueid type=\"local\" default=\"true\">episode-id</uniqueid>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("NFO does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<art>") {
		t.Fatal("non-Kodi art wrapper was emitted")
	}
}

func TestKodiNFOOmitsEmptyOptionalFields(t *testing.T) {
	text, err := nfoXML("movie", &Metadata{Title: "电影"}, "movie-id", nil, nil, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"<plot>", "<director>", "<genre>", "<tag>", "<thumb", "<fanart>"} {
		if strings.Contains(text, tag) {
			t.Fatalf("empty optional field %s was emitted:\n%s", tag, text)
		}
	}
	if !strings.Contains(text, "<movie>") || !strings.Contains(text, "<title>电影</title>") {
		t.Fatal(text)
	}
}

func TestKodiNFORejectsUnknownKindAndMissingTitle(t *testing.T) {
	if _, err := nfoXML("video", &Metadata{Title: "标题"}, "id", nil, nil, "", "", ""); err == nil {
		t.Fatal("unknown NFO root accepted")
	}
	if _, err := nfoXML("movie", &Metadata{}, "id", nil, nil, "", "", ""); err == nil {
		t.Fatal("missing title accepted")
	}
}
