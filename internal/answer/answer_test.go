package answer

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"  42  ":       "42",
		"42":           "42",
		"Привет Мир":   "привет мир",
		"привет   мир": "привет мир",
		"привет\tмир":  "привет мир",
		"привет\nмир":  "привет мир",
		"  a  b   c ":  "a b c",
		"":             "",
		"   ":          "",
		"3.14":         "3.14",
		"ОТВЕТ":        "ответ",
		" x y ":        "x y", // неразрывные пробелы
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

func TestEqual(t *testing.T) {
	equal := [][2]string{
		{"42", " 42 "},
		{"привет мир", "Привет   Мир"},
		{"1 2 3", "1\t2\n3"},
		{"AbC", "abc"},
	}
	for _, c := range equal {
		if !Equal(c[0], c[1]) {
			t.Errorf("Equal(%q, %q) = false, ожидалось true", c[0], c[1])
		}
	}
	notEqual := [][2]string{
		{"42", "43"},
		{"12", "1 2"},
		{"привет", "пока"},
		{"", "0"},
	}
	for _, c := range notEqual {
		if Equal(c[0], c[1]) {
			t.Errorf("Equal(%q, %q) = true, ожидалось false", c[0], c[1])
		}
	}
}
