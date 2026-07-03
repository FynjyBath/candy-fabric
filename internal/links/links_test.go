package links

import "testing"

func TestNormalizeOK(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"https://informatics.msk.ru/mod/statements/view.php?chapterid=113449", 113449},
		{"https://informatics.mccme.ru/mod/statements/view.php?chapterid=2958#1", 2958},
		{"http://www.informatics.msk.ru/mod/statements/view.php?chapterid=5", 5},
		{"https://WWW.INFORMATICS.MCCME.RU/MOD/STATEMENTS/VIEW.PHP?chapterid=7", 7},
		{"  https://informatics.msk.ru/mod/statements/view.php?foo=bar&chapterid=42&x=1  ", 42},
		{"https://informatics.msk.ru/mod/statements/view.php?CHAPTERID=99", 99},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if err != nil {
			t.Errorf("Normalize(%q): неожиданная ошибка %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %d, ожидалось %d", c.in, got, c.want)
		}
	}
}

func TestNormalizeErrors(t *testing.T) {
	cases := []string{
		"",
		"not a url",
		"ftp://informatics.msk.ru/mod/statements/view.php?chapterid=1",
		"https://example.com/mod/statements/view.php?chapterid=1",
		"https://informatics.msk.ru/mod/other/view.php?chapterid=1",
		"https://informatics.msk.ru/mod/statements/view.php",
		"https://informatics.msk.ru/mod/statements/view.php?chapterid=0",
		"https://informatics.msk.ru/mod/statements/view.php?chapterid=-3",
		"https://informatics.msk.ru/mod/statements/view.php?chapterid=abc",
	}
	for _, c := range cases {
		if _, err := Normalize(c); err == nil {
			t.Errorf("Normalize(%q): ожидалась ошибка, её нет", c)
		}
	}
}

func TestCanonicalURL(t *testing.T) {
	got := CanonicalURL("https://informatics.mccme.ru/", 123)
	want := "https://informatics.mccme.ru/mod/statements/view.php?chapterid=123"
	if got != want {
		t.Errorf("CanonicalURL = %q, ожидалось %q", got, want)
	}
	got = CanonicalURL("", 5)
	want = "https://informatics.msk.ru/mod/statements/view.php?chapterid=5"
	if got != want {
		t.Errorf("CanonicalURL с пустой базой = %q, ожидалось %q", got, want)
	}
}
