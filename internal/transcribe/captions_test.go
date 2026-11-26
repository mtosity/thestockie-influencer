package transcribe

import "testing"

func TestPickTrack(t *testing.T) {
	cases := []struct {
		name, lang        string
		manual, automatic []string
		want              string
		wantAuto          bool
	}{
		{"manual english wins", "en-US", []string{"en", "live_chat"}, []string{"en", "en-orig"}, "en", false},
		{"live_chat is not a transcript", "en-US", []string{"live_chat"}, []string{"de", "en", "en-US-orig"}, "en-US-orig", true},
		{"never the translated auto en", "en", nil, []string{"en", "en-orig", "fr"}, "en-orig", true},
		{"no orig track", "en", nil, []string{"en", "fr"}, "", false},
		{"non-english video goes to whisper", "de", nil, []string{"de-orig", "en"}, "", false},
		{"unknown language still uses en orig", "", nil, []string{"en-orig"}, "en-orig", true},
	}
	for _, c := range cases {
		got, auto := pickTrack(c.lang, c.manual, c.automatic)
		if got != c.want || auto != c.wantAuto {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.name, got, auto, c.want, c.wantAuto)
		}
	}
}

func TestVttToTextCollapsesRollingCues(t *testing.T) {
	vtt := `WEBVTT
Kind: captions
Language: en

00:00:00.000 --> 00:00:02.000 align:start position:0%
Nvidia<00:00:00.500><c> beat</c><00:00:01.000><c> estimates</c>

00:00:02.000 --> 00:00:02.010 align:start position:0%
Nvidia beat estimates

00:00:02.010 --> 00:00:04.000 align:start position:0%
Nvidia beat estimates
and<00:00:02.500><c> AT&amp;T</c><c> fell</c>
`
	if got, want := vttToText(vtt), "Nvidia beat estimates and AT&T fell"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
