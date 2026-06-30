package main

import "testing"

func TestRewriteHost(t *testing.T) {
	const base = "https://auth.your-tailnet.ts.net"
	for _, tc := range []struct {
		name string
		raw  string
		base string
		want string
	}{
		{
			name: "complete uri preserves path and user_code",
			raw:  "https://auth.example.com/device?user_code=ABCD-EFGH",
			base: base,
			want: "https://auth.your-tailnet.ts.net/device?user_code=ABCD-EFGH",
		},
		{
			name: "plain verification uri",
			raw:  "https://auth.example.com/device",
			base: base,
			want: "https://auth.your-tailnet.ts.net/device",
		},
		{
			name: "scheme and port come from base",
			raw:  "https://auth.example.com/device",
			base: "http://auth.your-tailnet.ts.net:8080",
			want: "http://auth.your-tailnet.ts.net:8080/device",
		},
		{
			name: "empty base is passthrough",
			raw:  "https://auth.example.com/device?user_code=X",
			base: "",
			want: "https://auth.example.com/device?user_code=X",
		},
		{
			name: "empty raw stays empty",
			raw:  "",
			base: base,
			want: "",
		},
		{
			name: "base without scheme/host is passthrough",
			raw:  "https://auth.example.com/device",
			base: "auth.your-tailnet.ts.net",
			want: "https://auth.example.com/device",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteHost(tc.raw, tc.base); got != tc.want {
				t.Errorf("rewriteHost(%q, %q) = %q, want %q", tc.raw, tc.base, got, tc.want)
			}
		})
	}
}
