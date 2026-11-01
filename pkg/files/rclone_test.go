package files

import "testing"

func TestParseCheck(t *testing.T) {
	out := `2026/11/02 10:00:00 ERROR : a.png: file not in S3 bucket dst
2026/11/02 10:00:00 NOTICE: S3 bucket dst: 1 files missing
2026/11/02 10:00:00 NOTICE: S3 bucket dst: 2 differences found
2026/11/02 10:00:00 NOTICE: S3 bucket dst: 2 errors while checking
2026/11/02 10:00:00 NOTICE: S3 bucket dst: 120 matching files`
	r := ParseCheck(out)
	if r.Missing != 1 || r.Differences != 2 || r.Matching != 120 {
		t.Fatalf("got %+v", r)
	}
	if z := ParseCheck("NOTICE: S3 bucket dst: 0 differences found\nNOTICE: S3 bucket dst: 5 matching files"); z.Differences != 0 || z.Matching != 5 {
		t.Fatalf("got %+v", z)
	}
}
