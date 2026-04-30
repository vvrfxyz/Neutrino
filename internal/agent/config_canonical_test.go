package agent

import "testing"

func TestConfigEqualCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{
			name: "identical",
			a:    `{"a":1,"b":2}`,
			b:    `{"a":1,"b":2}`,
			want: true,
		},
		{
			name: "key order differs",
			a:    `{"a":1,"b":2}`,
			b:    `{"b":2,"a":1}`,
			want: true,
		},
		{
			name: "whitespace differs",
			a:    `{"a":1,"b":2}`,
			b:    "{\n  \"a\": 1,\n  \"b\": 2\n}\n",
			want: true,
		},
		{
			name: "nested arrays preserve order",
			a:    `{"users":[{"id":1},{"id":2}]}`,
			b:    `{"users":[{"id":2},{"id":1}]}`,
			want: false,
		},
		{
			name: "values differ",
			a:    `{"a":1}`,
			b:    `{"a":2}`,
			want: false,
		},
		{
			name: "left invalid json",
			a:    `{not json`,
			b:    `{"a":1}`,
			want: false,
		},
		{
			name: "right invalid json",
			a:    `{"a":1}`,
			b:    `{}}}}`,
			want: false,
		},
		{
			name: "large integer preserved",
			a:    `{"port":18446744073709551615}`,
			b:    "{\n\t\"port\":18446744073709551615\n}",
			want: true,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := configEqualCanonical([]byte(tc.a), []byte(tc.b)); got != tc.want {
				t.Fatalf("configEqualCanonical=%v, want %v", got, tc.want)
			}
		})
	}
}
